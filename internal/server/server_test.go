package server

import (
	"context"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fjodor-rybakov/mini-pc-panel/internal/config"
	"github.com/fjodor-rybakov/mini-pc-panel/internal/metrics"
	"github.com/fjodor-rybakov/mini-pc-panel/internal/render"
)

const validDraft = `{"orientation":"landscape","screens":[{"id":1,"widgets":[{"id":1,"type":"text","rect":{"x":0,"y":0,"width":50,"height":16},"sensor":"cpu","format":"{0}"}]}]}`

// fakePanel implements Panel so the HTTP layer is exercised without any
// HID/serial hardware.
type fakePanel struct {
	dir       string
	cfg       *config.Config
	theme     *config.Theme
	swapped   int
	previews  int
	next      int
	shown     []int
	pauses    []bool
	lastLED   config.LEDSettings
	manualLED *config.LEDSettings
	ledErr    error
}

func newPanel(t *testing.T) *fakePanel {
	t.Helper()
	th, err := config.ParseTheme([]byte(validDraft))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	return &fakePanel{
		dir:   dir,
		cfg:   &config.Config{ThemeFile: filepath.Join(dir, "theme.json"), Dir: dir},
		theme: th,
	}
}

func (f *fakePanel) SensorNames() []string { return []string{"cpu", "memory"} }

func (f *fakePanel) LatestReadings() map[string]metrics.Reading {
	return map[string]metrics.Reading{
		"cpu":    {Value: 42.5, Max: 100, Text: []string{"42", "42°C"}, History: []float64{41, 42, 42.5}},
		"memory": {Value: 61.5, Max: 100, Text: []string{"62"}},
	}
}

func (f *fakePanel) Theme() *config.Theme    { return f.theme }
func (f *fakePanel) CurrentScreenIndex() int { return 0 }
func (f *fakePanel) Config() *config.Config  { return f.cfg }

func (f *fakePanel) ThemeFile() string { return f.cfg.ThemeFile }

func (f *fakePanel) SetThemeFile(path string) { f.cfg.ThemeFile = path }
func (f *fakePanel) SwapTheme(t *config.Theme) {
	f.theme = t
	f.swapped++
}

func (f *fakePanel) PreviewFrame(t *config.Theme, idx int) (*render.Frame, error) {
	if idx < 0 || idx >= len(t.Screens) {
		return nil, fmt.Errorf("screen %d out of range", idx)
	}
	return &render.Frame{Width: 320, Height: 170, Pixels: make([]byte, 320*170*2)}, nil
}

func (f *fakePanel) PreviewOnScreen(_ context.Context, t *config.Theme, idx int) (*render.Frame, error) {
	fr, err := f.PreviewFrame(t, idx)
	if err != nil {
		return nil, err
	}
	f.previews++
	return fr, nil
}

func (f *fakePanel) ApplyLEDManual(s config.LEDSettings) error {
	f.lastLED = s
	if f.ledErr == nil {
		f.manualLED = &s
	}
	return f.ledErr
}

// ManualLED mirrors app.App: a successful manual apply is what the daemon
// keeps honouring, and /api/state must echo it back to the editor.
func (f *fakePanel) ManualLED() (config.LEDSettings, bool) {
	if f.manualLED == nil {
		return config.LEDSettings{}, false
	}
	return *f.manualLED, true
}

func (f *fakePanel) NextScreen() { f.next++ }

// ShowScreen mirrors the real app.App.ShowScreen: only enabled screens are
// selectable by theme id.
func (f *fakePanel) ShowScreen(id int) bool {
	for _, sc := range f.theme.Screens {
		if sc.ID == id && sc.IsEnabled() {
			f.shown = append(f.shown, id)
			return true
		}
	}
	return false
}

func (f *fakePanel) PauseRotation(on bool) { f.pauses = append(f.pauses, on) }

func newTestServer(t *testing.T) (*httptest.Server, *fakePanel) {
	t.Helper()
	p := newPanel(t)
	s := New(p, "127.0.0.1:0")
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, p
}

func TestStateEndpoint(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var body struct {
		Sensors  []string `json:"sensors"`
		Readings map[string]struct {
			Value   float64   `json:"value"`
			Text    []string  `json:"text"`
			History []float64 `json:"history"`
		} `json:"readings"`
		Theme struct {
			Orientation string            `json:"orientation"`
			Screens     []json.RawMessage `json:"screens"`
		} `json:"theme"`
		CurrentScreen int `json:"current_screen"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Sensors) != 2 || body.Sensors[0] != "cpu" {
		t.Fatalf("sensors = %v", body.Sensors)
	}
	cpu := body.Readings["cpu"]
	if cpu.Value != 42.5 || len(cpu.Text) != 2 || cpu.Text[1] != "42°C" {
		t.Fatalf("cpu reading = %+v (REAL system numbers must be served)", cpu)
	}
	if len(cpu.History) != 3 {
		t.Fatalf("history = %v", cpu.History)
	}
	if body.Theme.Orientation != "landscape" || len(body.Theme.Screens) != 1 {
		t.Fatalf("theme = %+v", body.Theme)
	}
	if body.CurrentScreen != 0 {
		t.Fatalf("current_screen = %d", body.CurrentScreen)
	}
}

func TestPollEndpoint(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/api/poll")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Readings map[string]json.RawMessage `json:"readings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Readings) != 2 {
		t.Fatalf("poll readings = %v", body.Readings)
	}
}

// «примернить должно применяться к самому экрану»: a valid draft reaches
// PreviewOnScreen (the physical push) and the PNG reply is decodable.
func TestPreviewAppliesDraftToPanel(t *testing.T) {
	ts, p := newTestServer(t)
	resp, err := http.Post(ts.URL+"/api/preview?screen=0", "application/json", strings.NewReader(validDraft))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content-type %q, want image/png", ct)
	}
	cfg, err := png.DecodeConfig(resp.Body)
	if err != nil {
		t.Fatalf("preview body is not a PNG: %v", err)
	}
	if cfg.Width != 320 || cfg.Height != 170 {
		t.Fatalf("preview PNG %dx%d, want 320x170", cfg.Width, cfg.Height)
	}
	if p.previews != 1 {
		t.Fatalf("PreviewOnScreen calls = %d, want 1", p.previews)
	}
}

func TestPreviewRejections(t *testing.T) {
	ts, p := newTestServer(t)

	resp, err := http.Post(ts.URL+"/api/preview", "application/json", strings.NewReader("this is not json"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("garbage draft: status %d, want 422", resp.StatusCode)
	}

	resp, err = http.Post(ts.URL+"/api/preview", "application/json",
		strings.NewReader(`{"screens":[{"id":1,"widgets":[{"id":1,"type":"banana","rect":{"x":0,"y":0,"width":10,"height":10}}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("invalid theme draft: status %d, want 422", resp.StatusCode)
	}

	resp, err = http.Post(ts.URL+"/api/preview?screen=7", "application/json", strings.NewReader(validDraft))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("screen out of range: status %d, want 502", resp.StatusCode)
	}

	resp, err = http.Post(ts.URL+"/api/preview?screen=abc", "application/json", strings.NewReader(validDraft))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad screen param: status %d, want 400", resp.StatusCode)
	}

	if p.previews != 0 {
		t.Fatalf("rejected drafts must not reach the panel, got %d pushes", p.previews)
	}
}

func TestApplyHotSwapAndSave(t *testing.T) {
	ts, p := newTestServer(t)
	resp, err := http.Post(ts.URL+"/api/apply?save=1", "application/json", strings.NewReader(validDraft))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var body struct {
		OK    bool `json:"ok"`
		Saved bool `json:"saved"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.OK || !body.Saved {
		t.Fatalf("body = %+v", body)
	}
	if p.swapped != 1 {
		t.Fatalf("SwapTheme calls = %d, want 1 (hot swap)", p.swapped)
	}
	raw, err := os.ReadFile(p.Config().ThemeFile)
	if err != nil {
		t.Fatalf("?save=1 must persist the theme file: %v", err)
	}
	if !strings.Contains(string(raw), `"screens"`) {
		t.Fatalf("saved theme file unexpected content: %s", raw)
	}
}

func TestApplyRejectsInvalidDraft(t *testing.T) {
	ts, p := newTestServer(t)
	resp, err := http.Post(ts.URL+"/api/apply", "application/json", strings.NewReader(`{"screens":[{"id":1,"enabled":false}]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422", resp.StatusCode)
	}
	if p.swapped != 0 {
		t.Fatal("invalid draft must not swap the live theme")
	}
}

func TestLEDApply(t *testing.T) {
	ts, p := newTestServer(t)
	resp, err := http.Post(ts.URL+"/api/led", "application/json", strings.NewReader(`{"theme":2,"intensity":1,"speed":4}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if p.lastLED.Theme != 2 || p.lastLED.Intensity != 1 || p.lastLED.Speed != 4 {
		t.Fatalf("LED settings forwarded as %+v", p.lastLED)
	}

	// serial failure -> 502
	p.ledErr = fmt.Errorf("serial: port busy")
	resp, err = http.Post(ts.URL+"/api/led", "application/json", strings.NewReader(`{"theme":1,"intensity":3,"speed":3}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("LED error: status %d, want 502", resp.StatusCode)
	}
}

// The editor refills its LED form from /api/state on every page load. If
// that reply carries only config defaults, a manual "off" (theme 4) looks
// lost after a refresh and the next "apply" re-lights the strip - the
// state must echo the live manual settings, merged over the config block.
func TestStateEchoesManualLED(t *testing.T) {
	ts, p := newTestServer(t)
	p.cfg.LED.Port = "/dev/ttyUSB0"

	resp, err := http.Post(ts.URL+"/api/led", "application/json", strings.NewReader(`{"theme":4,"intensity":3,"speed":3}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("led apply status %d", resp.StatusCode)
	}

	resp, err = http.Get(ts.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Config struct {
			LED struct {
				Port      string `json:"port"`
				Theme     int    `json:"theme"`
				Intensity int    `json:"intensity"`
				Speed     int    `json:"speed"`
			} `json:"led"`
		} `json:"config"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Config.LED.Theme != 4 {
		t.Errorf("state led.theme = %d after manual off, want 4", body.Config.LED.Theme)
	}
	if body.Config.LED.Port != "/dev/ttyUSB0" {
		t.Errorf("state led.port = %q, manual merge must keep the config port", body.Config.LED.Port)
	}
}

func TestEditorStaticAsset(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "редактор виджетов") {
		t.Fatal("embedded editor (index.html) not served at /")
	}
}

// ---- theme picker and screen control (original GUI capabilities) ----

func TestThemesEndpoint(t *testing.T) {
	ts, p := newTestServer(t)
	alt := filepath.Join(p.dir, "alt.json")
	p.cfg.ThemeList = []config.ThemeRef{{Name: "Alt", File: alt}}
	resp, err := http.Get(ts.URL + "/api/themes")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var body struct {
		Themes    []config.ThemeRef `json:"themes"`
		ThemeFile string            `json:"theme_file"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Themes) != 1 || body.Themes[0].Name != "Alt" || body.Themes[0].File != alt {
		t.Fatalf("themes = %+v", body.Themes)
	}
	if body.ThemeFile != p.cfg.ThemeFile {
		t.Fatalf("theme_file = %q, want the live %q", body.ThemeFile, p.cfg.ThemeFile)
	}
}

// The editor switches to a theme from theme_list live: SwapTheme fires,
// the in-memory config follows, and nothing is written to disk.
func TestThemeSwitchLive(t *testing.T) {
	ts, p := newTestServer(t)
	alt := filepath.Join(p.dir, "alt.json")
	altJSON := `{"orientation":"portrait","screens":[{"id":7,"widgets":[{"id":1,"type":"text","rect":{"x":0,"y":0,"width":50,"height":16},"sensor":"cpu","format":"{0}"}]}]}`
	if err := os.WriteFile(alt, []byte(altJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	p.cfg.ThemeList = []config.ThemeRef{{Name: "Alt", File: alt}}

	reqBody, err := json.Marshal(map[string]string{"file": filepath.ToSlash(alt)})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(ts.URL+"/api/theme", "application/json", strings.NewReader(string(reqBody)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	var body struct {
		OK   bool   `json:"ok"`
		Name string `json:"name"`
		File string `json:"file"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.OK || body.Name != "Alt" || body.File != alt {
		t.Fatalf("body = %+v", body)
	}
	if p.swapped != 1 {
		t.Fatalf("SwapTheme calls = %d, want 1", p.swapped)
	}
	if len(p.theme.Screens) != 1 || p.theme.Screens[0].ID != 7 {
		t.Fatalf("live theme is not the switched one: %+v", p.theme.Screens)
	}
	if p.cfg.ThemeFile != alt {
		t.Fatalf("config theme_file = %q, want %q", p.cfg.ThemeFile, alt)
	}

	// name-based lookup must work too; unknown themes are 404, empty body 400.
	resp2, err := http.Post(ts.URL+"/api/theme", "application/json", strings.NewReader(`{"name":"Nope"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown theme: status %d, want 404", resp2.StatusCode)
	}
	resp3, err := http.Post(ts.URL+"/api/theme", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty switch: status %d, want 400", resp3.StatusCode)
	}
}

// POST /api/next_screen advances rotation; {"id":N} jumps straight to that
// screen id (original next_screen honours request.id), unknown ids 404.
func TestNextScreenAndJump(t *testing.T) {
	ts, p := newTestServer(t)

	resp, err := http.Post(ts.URL+"/api/next_screen", "application/json", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("advance: status %d", resp.StatusCode)
	}
	var body struct {
		OK    bool `json:"ok"`
		Index int  `json:"index"`
		ID    int  `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.OK || body.Index != 0 || body.ID != 1 {
		t.Fatalf("advance body = %+v", body)
	}
	if p.next != 1 {
		t.Fatalf("NextScreen calls = %d, want 1", p.next)
	}

	resp2, err := http.Post(ts.URL+"/api/next_screen", "application/json", strings.NewReader(`{"id":1}`))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("jump: status %d", resp2.StatusCode)
	}
	if p.next != 1 || len(p.shown) != 1 || p.shown[0] != 1 {
		t.Fatalf("jump must use ShowScreen only: next=%d shown=%v", p.next, p.shown)
	}

	resp3, err := http.Post(ts.URL+"/api/next_screen", "application/json", strings.NewReader(`{"id":99}`))
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown id: status %d, want 404", resp3.StatusCode)
	}
}

func TestPauseEndpoint(t *testing.T) {
	ts, p := newTestServer(t)
	for _, tc := range []struct {
		query string
		want  int
	}{
		{"?on=true", http.StatusOK},
		{"?on=false", http.StatusOK},
		{"?on=maybe", http.StatusBadRequest},
	} {
		resp, err := http.Post(ts.URL+"/api/pause"+tc.query, "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Fatalf("%s: status %d, want %d", tc.query, resp.StatusCode, tc.want)
		}
	}
	want := []bool{true, false}
	if len(p.pauses) != len(want) {
		t.Fatalf("PauseRotation calls = %v, want %v", p.pauses, want)
	}
	for i, w := range want {
		if p.pauses[i] != w {
			t.Fatalf("pauses[%d] = %v, want %v", i, p.pauses[i], w)
		}
	}
}
