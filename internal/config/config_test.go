package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHexUint16(t *testing.T) {
	var c struct {
		VID HexUint16 `json:"vid"`
		PID HexUint16 `json:"pid"`
	}
	if err := json.Unmarshal([]byte(`{"vid":"0x04D9","pid":64769}`), &c); err != nil {
		t.Fatal(err)
	}
	if c.VID != 0x04D9 || c.PID != 0xFD01 {
		t.Fatalf("got %04X/%04X, want 04D9/FD01", c.VID, c.PID)
	}
	if err := json.Unmarshal([]byte(`{"vid":"0xZZZZ"}`), &c); err == nil {
		t.Fatal("malformed hex must be rejected")
	}
	// 70000 used to truncate silently to 4464 — a typo would send the
	// daemon hunting for a random device instead of failing at load.
	for _, bad := range []string{`{"vid":70000}`, `{"vid":-1}`, `{"vid":1.5}`} {
		if err := json.Unmarshal([]byte(bad), &c); err == nil {
			t.Fatalf("%s must be rejected, not truncated", bad)
		}
	}
}

const minimalDraft = `{"screens":[{"id":1,"widgets":[{"id":1,"type":"text","rect":{"x":0,"y":0,"width":10,"height":10}}]}]}`

func TestLoadDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.json")
	if err := os.WriteFile(path, []byte(`{"theme_file":"themes/demo.json"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Dir != dir {
		t.Fatalf("Dir = %q, want %q", cfg.Dir, dir)
	}
	if want := filepath.Join(dir, "themes", "demo.json"); cfg.ThemeFile != want {
		t.Fatalf("ThemeFile = %q, want %q (resolved against config dir)", cfg.ThemeFile, want)
	}
	if cfg.Device.VID != 0x04D9 || cfg.Device.PID != 0xFD01 || cfg.Device.Interface != 1 {
		t.Fatalf("device defaults: %+v", cfg.Device)
	}
	if cfg.LED.Port != "/dev/ttyUSB0" || !cfg.LEDEnabled() {
		t.Fatalf("led defaults: %+v", cfg.LED)
	}
	if cfg.LED.Theme != 5 || cfg.LED.Intensity != 3 || cfg.LED.Speed != 3 {
		t.Fatalf("led settings defaults: %+v", cfg.LED.LEDSettings)
	}
	// Timing follows the original config.json semantics: heartbeat is the
	// idle threshold (60000), not a fixed send interval.
	if cfg.Poll != 1000 || cfg.Heartbeat != 60000 || cfg.RedrawDelay != 1600 || cfg.PacketDelay != 10 {
		t.Fatalf("timing defaults: %+v", []int{cfg.Poll, cfg.Heartbeat, cfg.RedrawDelay, cfg.PacketDelay})
	}
	if cfg.Listen != "0.0.0.0:8686" {
		t.Fatalf("Listen default = %q, want 0.0.0.0:8686", cfg.Listen)
	}
	if cfg.Sensors.MaxPoints != 300 || cfg.Sensors.Scaling != 1.5 {
		t.Fatalf("sensor defaults (original max_points/scale_factor): %+v", cfg.Sensors)
	}

	// explicit led enabled:false must be honoured
	off := filepath.Join(dir, "off.json")
	if err := os.WriteFile(off, []byte(`{"theme_file":"t.json","led":{"enabled":false}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg2, err := Load(off)
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.LEDEnabled() {
		t.Fatal(`"led":{"enabled":false} ignored`)
	}
}

func TestLoadErrors(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("missing config file must error")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "c.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "theme_file") {
		t.Fatalf("theme_file required; got %v", err)
	}
}

// "../" in a relative theme path used to be joined blindly, letting a
// config pull theme files (and editor writes) outside the config dir.
func TestLoadRejectsPathEscape(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.json")
	if err := os.WriteFile(path, []byte(`{"theme_file":"../evil.json"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("theme_file escape must be rejected; got %v", err)
	}
	// original theme_list spelling: the path lives under the "config" key
	if err := os.WriteFile(path, []byte(`{"theme_file":"ok.json","theme_list":[{"config":"../../etc/x.json"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("theme_list escape must be rejected; got %v", err)
	}
	// A nested but contained path stays fine.
	if err := os.WriteFile(path, []byte(`{"theme_file":"themes/./demo.json"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("contained path rejected: %v", err)
	}
}

// fix_value() clamps LED knobs silently, so out-of-range config values ran
// a different effect than the user wrote instead of failing at load.
func TestLoadRejectsBadLEDValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.json")
	for _, bad := range []string{
		`{"theme_file":"t.json","led":{"theme":9}}`,
		`{"theme_file":"t.json","led":{"speed":-2}}`,
		`{"theme_file":"t.json","led":{"intensity":7}}`,
	} {
		if err := os.WriteFile(path, []byte(bad), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("%s accepted; out-of-range led values must fail load", bad)
		}
	}
}

// Duplicate screen/widget ids made /api/next_screen?id=N and the editor
// ambiguous; the original addressed screens BY id.
func TestValidateDuplicateIDs(t *testing.T) {
	dup := `{"screens":[
		{"id":1,"widgets":[{"id":1,"type":"text","rect":{"x":0,"y":0,"width":10,"height":10}}]},
		{"id":1,"widgets":[{"id":1,"type":"text","rect":{"x":0,"y":0,"width":10,"height":10}}]}]}`
	if _, err := ParseTheme([]byte(dup)); err == nil || !strings.Contains(err.Error(), "duplicate screen id") {
		t.Fatalf("duplicate screen ids: %v", err)
	}
	dupW := `{"screens":[{"id":1,"widgets":[
		{"id":2,"type":"text","rect":{"x":0,"y":0,"width":10,"height":10}},
		{"id":2,"type":"text","rect":{"x":20,"y":0,"width":10,"height":10}}]}]}`
	if _, err := ParseTheme([]byte(dupW)); err == nil || !strings.Contains(err.Error(), "duplicate widget id") {
		t.Fatalf("duplicate widget ids: %v", err)
	}
}

// The original Node version's config spelling must load as-is: "theme"
// alias, numeric "refresh", "listen", theme_list with "config" file keys,
// and the sensors module array (ignored, Go derives its own list).
func TestLoadOriginalConfig(t *testing.T) {
	dir := t.TempDir()
	theme := filepath.Join(dir, "themes", "portrait_simple.json")
	if err := os.MkdirAll(filepath.Dir(theme), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(theme, []byte(`{"screens":[{"id":1,"duration":5000}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	draft := `{
	  "theme": "themes/portrait_simple.json",
	  "refresh": 0,
	  "listen": "0.0.0.0:8686",
	  "poll": 500,
	  "redraw_delay": 1600,
	  "heartbeat": 60000,
	  "theme_list": [{"name": "Simple", "config": "themes/portrait_simple.json"}],
	  "sensors": ["cpu_temp", "memory", "weather"]
	}`
	if err := os.WriteFile(path, []byte(draft), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("original-spelling config must load: %v", err)
	}
	if filepath.Clean(cfg.ThemeFile) != filepath.Clean(theme) {
		t.Fatalf(`"theme" alias -> ThemeFile = %q, want %q`, cfg.ThemeFile, theme)
	}
	if cfg.Poll != 500 || cfg.RedrawDelay != 1600 || cfg.Heartbeat != 60000 {
		t.Fatalf("original timings: %+v", []int{cfg.Poll, cfg.RedrawDelay, cfg.Heartbeat})
	}
	if cfg.Listen != "0.0.0.0:8686" {
		t.Fatalf("Listen = %q", cfg.Listen)
	}
	if len(cfg.ThemeList) != 1 || cfg.ThemeList[0].Name != "Simple" {
		t.Fatalf("theme_list: %+v", cfg.ThemeList)
	}
	if !strings.HasSuffix(cfg.ThemeList[0].File, "portrait_simple.json") {
		t.Fatalf("theme_list file not resolved: %q", cfg.ThemeList[0].File)
	}
	// The original sensors array is ignored; defaults apply instead.
	if len(cfg.Sensors.NetworkInterfaces) != 0 || cfg.Sensors.MaxPoints != 300 {
		t.Fatalf("sensors after ignoring original array: %+v", cfg.Sensors)
	}
	if _, err := LoadTheme(cfg.ThemeFile); err != nil {
		t.Fatalf("theme with original spelling must parse: %v", err)
	}
}

func TestParseThemeDefaults(t *testing.T) {
	th, err := ParseTheme([]byte(minimalDraft))
	if err != nil {
		t.Fatal(err)
	}
	if th.Orientation != "landscape" || th.Refresh != "update" || th.Background != "#000000" {
		t.Fatalf("theme defaults: %+v", th)
	}
	w, h, portrait := th.OrientationSize()
	if w != 320 || h != 170 || portrait {
		t.Fatalf("landscape size: %dx%d portrait=%v", w, h, portrait)
	}
	sc := th.Screens[0]
	// Duration 0/absent = never rotate (main.js line 271), NOT a 10000 default.
	if !sc.IsEnabled() || sc.Duration != 0 {
		t.Fatalf("screen defaults: %+v", sc)
	}
	widget := sc.Widgets[0]
	// text.js falls back per-draw (color || 'white'); only the font size is
	// defaulted here (text.js '20px Arial').
	if widget.Type != "text" || widget.Color != "" || widget.Font.Size != 20 {
		t.Fatalf("widget defaults: %+v", widget)
	}
}

func TestParseThemePortraitSize(t *testing.T) {
	th, err := ParseTheme([]byte(`{"orientation":"portrait","screens":[{"id":1}]}`))
	if err != nil {
		t.Fatal(err)
	}
	w, h, portrait := th.OrientationSize()
	if w != 170 || h != 320 || !portrait {
		t.Fatalf("portrait size: %dx%d portrait=%v", w, h, portrait)
	}
}

func TestParseThemeRejections(t *testing.T) {
	cases := map[string]string{
		"unknown widget type":  `{"screens":[{"id":1,"widgets":[{"id":1,"type":"banana","rect":{"x":0,"y":0,"width":10,"height":10}}]}]}`,
		"rect beyond screen":   `{"screens":[{"id":1,"widgets":[{"id":1,"type":"text","rect":{"x":300,"y":0,"width":100,"height":16}}]}]}`,
		"all screens disabled": `{"screens":[{"id":1,"enabled":false}]}`,
		"no screens":           `{"screens":[]}`,
	}
	for name, draft := range cases {
		if _, err := ParseTheme([]byte(draft)); err == nil {
			t.Errorf("%s: draft accepted, want rejection", name)
		}
	}
}

// The original theme spelling must load unchanged: widget type in "name",
// "sensor": true + "value", CSS "font", the "duartion" typo, and
// main.js-style id ordering.
func TestOriginalThemeSpelling(t *testing.T) {
	draft := `{
	  "orientation": "portrait",
	  "screens": [
	    {"id": 2, "duartion": 0, "widgets": []},
	    {"id": 1, "duartion": 5000, "widgets": [
	      {"id": 2, "name": "text", "sensor": true, "value": "cpu_usage", "format": "{0}%",
	       "font": "17px Arial", "color": "#878787", "rect": {"x": 0, "y": 0, "width": 60, "height": 16}},
	      {"id": 3, "name": "text", "sensor": false, "value": "Hello", "font": "bold 18px Arial",
	       "rect": {"x": 0, "y": 50, "width": 60, "height": 18}}
	    ]}
	  ]
	}`
	th, err := ParseTheme([]byte(draft))
	if err != nil {
		t.Fatal(err)
	}
	// main.js line 634: screens are ordered by id.
	if len(th.Screens) != 2 || th.Screens[0].ID != 1 || th.Screens[1].ID != 2 {
		t.Fatalf("screens not sorted by id: %+v", th.Screens)
	}
	if th.Screens[0].Duration != 5000 {
		t.Fatalf(`"duartion" alias: Duration = %d, want 5000`, th.Screens[0].Duration)
	}
	if th.Screens[1].Duration != 0 {
		t.Fatalf(`"duartion": 0 must mean never-rotate, got %d`, th.Screens[1].Duration)
	}
	widgets := th.Screens[0].Widgets
	if len(widgets) != 2 || widgets[0].ID != 2 || widgets[1].ID != 3 {
		t.Fatalf("widgets not sorted by id: %+v", widgets)
	}
	text := widgets[0] // id 2
	if text.Type != "text" || text.Sensor != "cpu_usage" || text.Value != "" {
		t.Fatalf(`"sensor": true must move "value" into Sensor: %+v`, text)
	}
	if text.Font.Family != "Arial" || text.Font.Size != 17 || text.Font.Bold {
		t.Fatalf(`CSS font "17px Arial": %+v`, text.Font)
	}
	if text.Color != "#878787" {
		t.Fatalf("color must be kept verbatim: %q", text.Color)
	}
	literal := widgets[1] // id 3
	if literal.Sensor != "" || literal.Value != "Hello" {
		t.Fatalf(`"sensor": false must keep "value" literal: %+v`, literal)
	}
	if !literal.Font.Bold || literal.Font.Size != 18 {
		t.Fatalf(`CSS font "bold 18px Arial": %+v`, literal.Font)
	}
}

func TestScreenDurationAlias(t *testing.T) {
	// "duration" wins over the "duartion" typo when both are present.
	th, err := ParseTheme([]byte(`{"screens":[{"id":1,"duration":7000,"duartion":5000}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if th.Screens[0].Duration != 7000 {
		t.Fatalf("duration = %d, want 7000", th.Screens[0].Duration)
	}
}

func TestFontSpecObjectForm(t *testing.T) {
	var f FontSpec
	if err := json.Unmarshal([]byte(`{"family":"DejaVu","size":12.5,"bold":true}`), &f); err != nil {
		t.Fatal(err)
	}
	if f.Family != "DejaVu" || f.Size != 12.5 || !f.Bold {
		t.Fatalf("object font: %+v", f)
	}
	if err := json.Unmarshal([]byte(`"13px"`), &f); err == nil {
		t.Fatal("garbage CSS font must be rejected")
	}
}

// The shipped examples must pass the production parsers at their real
// relative pairing (config.example.json -> theme_file -> themes/demo.json).
func TestShippedExamplesValidate(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatalf("config.example.json: %v", err)
	}
	if _, err := LoadTheme(cfg.ThemeFile); err != nil {
		t.Fatalf("theme_file %s: %v", cfg.ThemeFile, err)
	}
}

func TestSaveThemeRoundTrip(t *testing.T) {
	th, err := ParseTheme([]byte(minimalDraft))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "theme.json")
	if err := SaveTheme(path, th); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(raw) {
		t.Fatalf("SaveTheme wrote invalid JSON: %s", raw)
	}
	got, err := LoadTheme(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Orientation != th.Orientation || len(got.Screens) != 1 {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if got.Screens[0].Widgets[0].Rect != th.Screens[0].Widgets[0].Rect {
		t.Fatalf("rect lost in round trip: %+v", got.Screens[0].Widgets[0].Rect)
	}
	if got.Screens[0].Duration != th.Screens[0].Duration {
		t.Fatalf("duration lost in round trip: %d vs %d", got.Screens[0].Duration, th.Screens[0].Duration)
	}
}
