package app

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/fjodor-rybakov/mini-pc-panel/internal/config"
	"github.com/fjodor-rybakov/mini-pc-panel/internal/display"
	"github.com/fjodor-rybakov/mini-pc-panel/internal/led"
	"github.com/fjodor-rybakov/mini-pc-panel/internal/metrics"
	"github.com/fjodor-rybakov/mini-pc-panel/internal/render"
)

// captureTransport records every packet the display worker puts on the wire,
// so tests can assert real queue -> wire behaviour without a panel.
type captureTransport struct {
	mu   sync.Mutex
	pkts [][]byte
}

func (c *captureTransport) Write(p []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pkts = append(c.pkts, append([]byte(nil), p...))
	return nil
}

func (c *captureTransport) Close() error { return nil }

func (c *captureTransport) snapshots() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.pkts...)
}

func countRedraw(pkts [][]byte) int {
	n := 0
	for _, p := range pkts {
		if len(p) > 1 && p[0] == 0x55 && p[1] == display.CmdRedraw {
			n++
		}
	}
	return n
}

// User report "Экран не выключается": proves the button path end to end.
// Standby(true) must make the very next tick push ONE full BLACK redraw;
// further standby ticks stay silent; leaving standby latches a full
// repaint so the live frame returns over the black one.
func TestStandbyTickBlacksPanel(t *testing.T) {
	tr := &captureTransport{}
	ctx := context.Background()
	disp := display.New(tr, 0, 0)
	disp.Start(ctx) // app.Run does this; the test drives tick() directly
	t.Cleanup(func() { _ = disp.Close() })
	a := &App{
		cfg:      &config.Config{},
		registry: metrics.NewRegistry(),
		theme:    &config.Theme{Screens: []config.Screen{{ID: 1, Duration: 10000}}},
		disp:     disp,
	}

	a.Standby(true)
	if err := a.tick(ctx); err != nil {
		t.Fatalf("standby entry tick: %v", err)
	}

	// The redraw posts as one burst job; wait for all 27 chunks to land.
	deadline := time.Now().Add(3 * time.Second)
	for {
		pk := tr.snapshots()
		if countRedraw(pk) >= display.ChunkCount {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("standby black redraw: only %d/%d chunks reached the wire", countRedraw(pk), display.ChunkCount)
		}
		time.Sleep(5 * time.Millisecond)
	}
	for _, p := range tr.snapshots() {
		if len(p) < 9 || p[1] != display.CmdRedraw {
			continue
		}
		for _, b := range p[8:] {
			if b != 0 {
				t.Fatal("standby redraw payload is not black")
			}
		}
	}

	// Applied once already: further standby ticks must not spam redraws.
	first := countRedraw(tr.snapshots())
	if err := a.tick(ctx); err != nil {
		t.Fatalf("second standby tick: %v", err)
	}
	if got := countRedraw(tr.snapshots()); got != first {
		t.Fatalf("second standby tick re-sent a full redraw: %d -> %d chunks", first, got)
	}

	// Exit: the panel holds a black frame lastFrame knows nothing about,
	// so the exit tick must full-repaint the live frame. The worker waits
	// redrawDelay (1.6 s; New clamps it) after a burst, so wait for the
	// second 27-chunk job instead of asserting right after the tick.
	a.Standby(false)
	if err := a.tick(ctx); err != nil {
		t.Fatalf("exit tick: %v", err)
	}
	if a.lastFrame == nil {
		t.Fatal("exit tick must repaint the live frame (lastFrame still nil)")
	}
	deadline = time.Now().Add(8 * time.Second)
	for {
		if n := countRedraw(tr.snapshots()); n >= 2*display.ChunkCount {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("exit full redraw: chunks %d, want >= %d", countRedraw(tr.snapshots()), 2*display.ChunkCount)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Field regression "Экран не выключается / Ничего не поменялось" (Nov 2026):
// the editor's «Примерить на экран» fired a SECOND full redraw while standby
// was on (log showed "redraw: all 27 chunks" 6 s after "standby on"),
// painting the live draft back over the black frame. PreviewOnScreen must
// refuse the panel push while standby is on — the browser still gets its
// PNG frame — and the standby tick must own the single black redraw.
func TestPreviewDuringStandbyKeepsPanelBlack(t *testing.T) {
	tr := &captureTransport{}
	ctx := context.Background()
	disp := display.New(tr, 0, 0)
	disp.Start(ctx)
	t.Cleanup(func() { _ = disp.Close() })
	a := &App{
		cfg:       &config.Config{},
		registry:  metrics.NewRegistry(),
		theme:     &config.Theme{Screens: []config.Screen{{ID: 1, Duration: 10000}}},
		fonts:     render.NewFonts(nil),
		disp:      disp,
		standbyOn: true,
	}

	if _, err := a.PreviewOnScreen(ctx, a.theme, 0); err != nil {
		t.Fatalf("PreviewOnScreen during standby: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if n := countRedraw(tr.snapshots()); n != 0 {
		t.Fatalf("preview during standby pushed %d redraw chunk(s) over the black frame", n)
	}

	// Standby still owns the screen: the entry tick blacks it once.
	if err := a.tick(ctx); err != nil {
		t.Fatalf("standby tick after refused preview: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for countRedraw(tr.snapshots()) < display.ChunkCount {
		if time.Now().After(deadline) {
			t.Fatalf("standby black redraw: only %d/%d chunks on the wire", countRedraw(tr.snapshots()), display.ChunkCount)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// "Вынеси в настройку управление временем": the editor installs the
// standby timer at runtime through SetStandbySchedule (no button press).
// A window covering "now" must open standby on the very next poll tick and
// black the panel exactly like the button path; clearing the schedule must
// land in config.json as "no timer" - afterwards only the button switches
// the screen off and nothing flips it back.
func TestStandbyScheduleFromSettingsBlacksPanel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"theme_file":"t.json"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	tr := &captureTransport{}
	ctx := context.Background()
	disp := display.New(tr, 0, 0)
	disp.Start(ctx)
	t.Cleanup(func() { _ = disp.Close() })
	a := &App{
		cfg:      cfg,
		registry: metrics.NewRegistry(),
		theme:    &config.Theme{Screens: []config.Screen{{ID: 1, Duration: 10000}}},
		fonts:    render.NewFonts(nil),
		disp:     disp,
	}

	// Dynamic +/-2-minute window: near midnight it wraps, and Active()
	// treats [start, end) past midnight as still containing "now".
	now := time.Now()
	sc := config.StandbyConfig{
		Start: now.Add(-2 * time.Minute).Format("15:04"),
		End:   now.Add(2 * time.Minute).Format("15:04"),
	}
	if err := a.SetStandbySchedule(sc); err != nil {
		t.Fatalf("SetStandbySchedule: %v", err)
	}
	if err := a.tick(ctx); err != nil {
		t.Fatalf("tick with a live schedule window: %v", err)
	}
	if !a.StandbyState() {
		t.Fatal("a schedule window covering now must switch standby on")
	}
	deadline := time.Now().Add(3 * time.Second)
	for countRedraw(tr.snapshots()) < display.ChunkCount {
		if time.Now().After(deadline) {
			t.Fatalf("scheduled standby: only %d/%d chunks on the wire", countRedraw(tr.snapshots()), display.ChunkCount)
		}
		time.Sleep(5 * time.Millisecond)
	}

	if err := a.SetStandbySchedule(config.StandbyConfig{}); err != nil {
		t.Fatalf("clearing the timer: %v", err)
	}
	if a.StandbySchedule().Enabled() {
		t.Fatal("cleared schedule must report Enabled() false")
	}
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.json after clearing the timer: %v", err)
	}
	if reloaded.Standby.Enabled() {
		t.Fatalf("config.json still carries a standby window: %+v", reloaded.Standby)
	}
}

func TestDeviceRectLandscapeIsIdentity(t *testing.T) {
	r := config.Rect{X: 3, Y: 4, Width: 5, Height: 6}
	if got := DeviceRect(r, false); got != r {
		t.Fatalf("landscape: got %+v, want identity %+v", got, r)
	}
}

// Portrait: the logical layout is 170x320 but the wire buffer is ALWAYS
// 320x170 (spec §3), so widget rects are pre-rotated into device space:
// X' = Y, Y' = 170 - (X + Width), W' = Height, H' = Width.
func TestDeviceRectPortrait(t *testing.T) {
	r := config.Rect{X: 10, Y: 20, Width: 100, Height: 30}
	want := config.Rect{X: 20, Y: 60, Width: 30, Height: 100}
	if got := DeviceRect(r, true); got != want {
		t.Fatalf("portrait: got %+v, want %+v", got, want)
	}
	// The full portrait screen must map exactly onto the full 320x170 buffer.
	full := config.Rect{X: 0, Y: 0, Width: 170, Height: 320}
	wantFull := config.Rect{X: 0, Y: 0, Width: 320, Height: 170}
	if got := DeviceRect(full, true); got != wantFull {
		t.Fatalf("portrait full screen: got %+v, want %+v", got, wantFull)
	}
}

// Regression for the hot-swap panic: the poll tick reads themeDirty, then
// /api/theme swaps in a SHORTER screens list before rotateScreen runs with
// the stale index — screens[stale] panicked (no recover anywhere). The
// swap now resets screenIdx, and rotateScreen clamps defensively.
func TestSwapThemeShorterScreensDoNotPanic(t *testing.T) {
	big := &config.Theme{Screens: []config.Screen{
		{ID: 1, Duration: 10000}, {ID: 2, Duration: 10000}, {ID: 3, Duration: 10000},
	}}
	small := &config.Theme{Screens: []config.Screen{
		{ID: 1, Duration: 10000}, {ID: 2, Duration: 10000},
	}}
	// Empty registry: SwapTheme re-checks sensor names and dereferences it.
	a := &App{cfg: &config.Config{}, registry: metrics.NewRegistry(), theme: big, screenIdx: 2}

	// The exact race window: tick latched dirty=false, then the swap landed.
	a.theme = small
	a.rotateScreen() // used to panic: small.Screens[2]
	if a.screenIdx < 0 || a.screenIdx >= len(small.Screens) {
		t.Fatalf("stale index survived rotateScreen: %d", a.screenIdx)
	}

	a.SwapTheme(small)
	if a.screenIdx != 0 {
		t.Fatalf("SwapTheme must reset screenIdx, got %d", a.screenIdx)
	}
}

// Regression for "LED off doesn't work": an "off" pushed from the editor
// used to be undone by the very next screen rotation, which re-applied the
// config's global theme. Manual settings must be the live global instead
// (a screen's own led_config still wins), and theme 0 — which the old code
// silently clamped into rainbow — must be rejected. led.Device{} has no
// serial port, so every wire write is a silent no-op: no hardware needed.
func TestManualLEDOffSurvivesRotation(t *testing.T) {
	cfg := &config.Config{}
	on := true
	cfg.LED.Enabled = &on
	cfg.LED.LEDSettings = config.LEDSettings{Theme: 5, Intensity: 3, Speed: 3}
	a := &App{cfg: cfg, led: &led.Device{}}

	if err := a.ApplyLEDManual(config.LEDSettings{Theme: led.ThemeOff, Intensity: 3, Speed: 3}); err != nil {
		t.Fatalf("ApplyLEDManual(off): %v", err)
	}
	if a.lastLEDTheme != led.ThemeOff {
		t.Fatalf("after manual off: lastLEDTheme = %d, want %d", a.lastLEDTheme, led.ThemeOff)
	}
	if err := a.ApplyLEDManual(config.LEDSettings{Theme: 0}); err == nil {
		t.Fatal("theme 0 must be rejected, not sent as rainbow")
	}
	if a.manualLED == nil || a.manualLED.Theme != led.ThemeOff {
		t.Fatalf("rejected call must not replace the manual settings: %+v", a.manualLED)
	}

	// The rotation path that used to re-apply config theme 5 over the off.
	a.applyScreenLED(&config.Screen{ID: 1, Name: "s"})
	if a.lastLEDTheme != led.ThemeOff {
		t.Fatalf("after rotation: lastLEDTheme = %d, want %d — manual off must outrank the config theme",
			a.lastLEDTheme, led.ThemeOff)
	}

	// A screen with its own led_config still outranks the manual global.
	sc := &config.Screen{ID: 2, Name: "s2", LED: &config.LEDSettings{Theme: led.ThemeRainbow, Intensity: 2, Speed: 2}}
	a.applyScreenLED(sc)
	if a.lastLEDTheme != led.ThemeRainbow {
		t.Fatalf("screen led_config must outrank manual settings: lastLEDTheme = %d, want %d",
			a.lastLEDTheme, led.ThemeRainbow)
	}
}
