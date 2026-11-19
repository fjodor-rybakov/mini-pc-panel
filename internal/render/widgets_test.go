package render

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/fjodor-rybakov/mini-pc-panel/internal/config"
	"github.com/fjodor-rybakov/mini-pc-panel/internal/metrics"
)

// Pixel-level fidelity tests for the widget renderers ported from the
// original s1panel/widgets/*.js. render_test.go covers the frame plumbing;
// these lock the Chart.js/custom-canvas semantics that were lost in port.

func rgb565Of(r, g, b uint8) []byte {
	v := (uint16(r>>3) << 11) | (uint16(g>>2) << 5) | uint16(b>>3)
	return []byte{byte(v >> 8), byte(v & 0xFF)}
}

func pixelAt(f *Frame, x, y int) []byte {
	off := (y*f.Width + x) * 2
	return f.Pixels[off : off+2]
}

func decode565(b []byte) (r5, g6, b5 uint16) {
	v := uint16(b[0])<<8 | uint16(b[1])
	return v >> 11, (v >> 5) & 0x3F, v & 0x1F
}

func writeSolidPNG(t *testing.T, path string, n int, c color.Color) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, n, n))
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			img.Set(x, y, c)
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

// TestDoughnutChartDefaults locks the doughnut_chart.js defaults: rotation
// 225, circumference 270, cutout 80%, used #48BB78, free #EDF2F7. Widget
// rect 40x40 at the origin -> cx=cy=20, rOut=20, rIn=16. Chart.js angles
// are degrees clockwise from 12 o'clock.
func TestDoughnutChartDefaults(t *testing.T) {
	draft := `{"orientation":"landscape","background":"#000000","screens":[{"id":1,"widgets":[
		{"id":1,"type":"doughnut_chart","sensor":"cpu","rect":{"x":0,"y":0,"width":40,"height":40}}]}]}`
	th, err := config.ParseTheme([]byte(draft))
	if err != nil {
		t.Fatal(err)
	}
	rd := map[string]metrics.Reading{"cpu": {Value: 50, Min: 0, Max: 100}}
	f, err := Render(&th.Screens[0], th, 320, 170, false, rd, NewFonts(nil))
	if err != nil {
		t.Fatal(err)
	}
	// used half: rotation 225 covers CW 225..315, value 50% extends it to
	// CW 225..360; sample at CW 300 on the ring mid-radius (18).
	if got, want := pixelAt(f, 4, 11), rgb565Of(0x48, 0xBB, 0x78); !bytes.Equal(got, want) {
		t.Fatalf("used arc at CW 300: got %02X %02X want %02X %02X", got[0], got[1], want[0], want[1])
	}
	// free half: sample at CW 45.
	if got, want := pixelAt(f, 32, 7), rgb565Of(0xED, 0xF2, 0xF7); !bytes.Equal(got, want) {
		t.Fatalf("free arc at CW 45: got %02X %02X want %02X %02X", got[0], got[1], want[0], want[1])
	}
	// the 90-degree gap at CW 180 (bottom) shows the background.
	if got, want := pixelAt(f, 20, 38), rgb565Of(0x00, 0x00, 0x00); !bytes.Equal(got, want) {
		t.Fatalf("gap at CW 180: got %02X %02X want %02X %02X", got[0], got[1], want[0], want[1])
	}
	// the hole is a real hole (no punched-out disc): centre stays background.
	if got, want := pixelAt(f, 20, 20), rgb565Of(0x00, 0x00, 0x00); !bytes.Equal(got, want) {
		t.Fatalf("ring centre: got %02X %02X want %02X %02X", got[0], got[1], want[0], want[1])
	}
}

// TestCustomBarGradients locks custom_bar.js: full-rect vertical gradient
// #404040->#000000 for the track and lightgreen->darkgreen for the first
// value% of the width (value 50 -> left half green, right half grey).
func TestCustomBarGradients(t *testing.T) {
	draft := `{"orientation":"landscape","background":"#000000","screens":[{"id":1,"widgets":[
		{"id":1,"type":"custom_bar","sensor":"pct","rect":{"x":0,"y":0,"width":40,"height":20}}]}]}`
	th, err := config.ParseTheme([]byte(draft))
	if err != nil {
		t.Fatal(err)
	}
	rd := map[string]metrics.Reading{"pct": {Value: 50}}
	f, err := Render(&th.Screens[0], th, 320, 170, false, rd, NewFonts(nil))
	if err != nil {
		t.Fatal(err)
	}
	// inside the filled half: green channel dominates
	r5, g6, b5 := decode565(pixelAt(f, 10, 10))
	if !(g6>>1 > r5 && g6>>1 > b5) {
		t.Fatalf("fill pixel: r5=%d g6=%d b5=%d; expected green-dominant gradient", r5, g6, b5)
	}
	// outside the filled half: the grey track gradient, equal channels
	r5, g6, b5 = decode565(pixelAt(f, 39, 10))
	if r5 != b5 || absDiff(int(g6>>1), int(r5)) > 1 || r5 == 0 || r5 > 8 {
		t.Fatalf("track pixel: r5=%d g6=%d b5=%d; expected dark grey gradient", r5, g6, b5)
	}
}

func absDiff(a, b int) int {
	if a < b {
		return b - a
	}
	return a - b
}

// TestImageWidgetNativeSizeAndCrop locks image.js: drawImage(image,
// rect.x, rect.y) at native size clipped to the rect - an 8x8 picture in a
// 5x5 rect covers exactly the rect's top-left 5x5, nothing beyond it.
func TestImageWidgetNativeSizeAndCrop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pic.png")
	writeSolidPNG(t, path, 8, color.White)
	draft := fmt.Sprintf(`{"orientation":"landscape","background":"#000000","screens":[{"id":1,"widgets":[
		{"id":1,"type":"image","value":"%s","rect":{"x":100,"y":80,"width":5,"height":5}}]}]}`,
		filepath.ToSlash(path))
	th, err := config.ParseTheme([]byte(draft))
	if err != nil {
		t.Fatal(err)
	}
	f, err := Render(&th.Screens[0], th, 320, 170, false, nil, NewFonts(nil))
	if err != nil {
		t.Fatal(err)
	}
	white := rgb565Of(0xFF, 0xFF, 0xFF)
	black := rgb565Of(0x00, 0x00, 0x00)
	for _, s := range []struct{ x, y int }{{102, 82}, {104, 84}} {
		if got := pixelAt(f, s.x, s.y); !bytes.Equal(got, white) {
			t.Fatalf("inside crop (%d,%d): got %02X %02X want white", s.x, s.y, got[0], got[1])
		}
	}
	for _, s := range []struct{ x, y int }{{106, 84}, {102, 86}} {
		if got := pixelAt(f, s.x, s.y); !bytes.Equal(got, black) {
			t.Fatalf("outside rect (%d,%d): got %02X %02X want background", s.x, s.y, got[0], got[1])
		}
	}
}

// TestWallpaperCoversScreen locks the original wallpaper draw: the picture
// is scaled to cover the whole canvas (centred), drawn before widgets.
func TestWallpaperCoversScreen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wall.png")
	writeSolidPNG(t, path, 8, color.White)
	draft := fmt.Sprintf(`{"orientation":"landscape","background":"#000000","screens":[{"id":1,"wallpaper":%q,"widgets":[
		{"id":1,"type":"none","rect":{"x":0,"y":0,"width":10,"height":10}}]}]}`,
		filepath.ToSlash(path))
	th, err := config.ParseTheme([]byte(draft))
	if err != nil {
		t.Fatal(err)
	}
	f, err := Render(&th.Screens[0], th, 320, 170, false, nil, NewFonts(nil))
	if err != nil {
		t.Fatal(err)
	}
	white := rgb565Of(0xFF, 0xFF, 0xFF)
	for _, s := range []struct{ x, y int }{{0, 0}, {319, 0}, {0, 169}, {319, 169}} {
		if got := pixelAt(f, s.x, s.y); !bytes.Equal(got, white) {
			t.Fatalf("wallpaper corner (%d,%d): got %02X %02X want white", s.x, s.y, got[0], got[1])
		}
	}
}
