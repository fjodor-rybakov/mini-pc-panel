package app

import (
	"testing"

	"github.com/fjodor-rybakov/mini-pc-panel/internal/config"
	"github.com/fjodor-rybakov/mini-pc-panel/internal/led"
	"github.com/fjodor-rybakov/mini-pc-panel/internal/metrics"
)

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
