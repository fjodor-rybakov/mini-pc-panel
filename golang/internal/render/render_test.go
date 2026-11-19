package render

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"

	"github.com/fjodor-rybakov/mini-pc-panel/golang/internal/config"
	"github.com/fjodor-rybakov/mini-pc-panel/golang/internal/metrics"
)

// Spec §3: RGB565 big-endian byte order.
func TestRGB565FromRGBA(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 3, 1))
	img.SetRGBA(0, 0, color.RGBA{255, 0, 0, 255})
	img.SetRGBA(1, 0, color.RGBA{0, 255, 0, 255})
	img.SetRGBA(2, 0, color.RGBA{0, 0, 255, 255})
	got := RGB565FromRGBA(img)
	want := []byte{0xF8, 0x00, 0x07, 0xE0, 0x00, 0x1F} // R, G, B
	if !bytes.Equal(got, want) {
		t.Fatalf("got % X, want % X", got, want)
	}
}

func TestFormatTokens(t *testing.T) {
	rd := map[string]metrics.Reading{
		"cpu": {Value: 42.4, Text: []string{"42", "42°C"}},
	}
	cases := []struct {
		name string
		w    config.Widget
		want string
	}{
		{"{v} integer", config.Widget{Sensor: "cpu", Format: "{v}%"}, "42%"},
		{"{v1} one decimal", config.Widget{Sensor: "cpu", Format: "{v1}"}, "42.4"},
		{"{0}..{9} text tokens", config.Widget{Sensor: "cpu", Format: "{0}/{1}/{5}"}, "42/42°C/"},
		{"empty format uses Text[0]", config.Widget{Sensor: "cpu"}, "42"},
		{"unsampled sensor", config.Widget{Sensor: "missing", Format: "{v}"}, "N/A"},
		{"literal value", config.Widget{Value: "hello"}, "hello"},
		{"unknown token vanishes", config.Widget{Sensor: "cpu", Format: "a{x}b"}, "ab"},
	}
	for _, c := range cases {
		if got := FormatTokens(c.w, rd); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestFrameRegionAndSameRegion(t *testing.T) {
	f := &Frame{Width: 4, Height: 2, Pixels: make([]byte, 4*2*2)}
	for i := range f.Pixels {
		f.Pixels[i] = byte(i)
	}
	// row 1 starts at byte (1*4+0)*2 = 8; x=1..2 -> bytes 10..13
	if got := f.Region(1, 1, 2, 1); !bytes.Equal(got, []byte{10, 11, 12, 13}) {
		t.Fatalf("Region = %v, want [10 11 12 13]", got)
	}
	if !f.SameRegion(f, 1, 1, 2, 1) {
		t.Fatal("frame must equal itself")
	}
	g := &Frame{Width: 4, Height: 2, Pixels: append([]byte(nil), f.Pixels...)}
	g.Pixels[12] ^= 0xFF
	if f.SameRegion(g, 1, 1, 2, 1) {
		t.Fatal("change inside the region must be detected")
	}
	h := &Frame{Width: 4, Height: 2, Pixels: append([]byte(nil), f.Pixels...)}
	h.Pixels[0] ^= 0xFF // outside region (0,1)-(2,1)
	if !f.SameRegion(h, 1, 1, 2, 1) {
		t.Fatal("change outside the region must NOT dirty it (poll optimisation)")
	}
}

const landscapeDraft = `{"orientation":"landscape","screens":[{"id":1,"widgets":[{"id":1,"type":"text","rect":{"x":10,"y":10,"width":100,"height":20},"sensor":"cpu","format":"{0}"}]}]}`
const portraitDraft = `{"orientation":"portrait","screens":[{"id":1,"widgets":[{"id":1,"type":"text","rect":{"x":10,"y":10,"width":100,"height":20},"sensor":"cpu","format":"{0}"}]}]}`

func sampleReadings() map[string]metrics.Reading {
	return map[string]metrics.Reading{"cpu": {Value: 42, Text: []string{"42"}}}
}

func nonBlank(f *Frame) bool {
	for _, b := range f.Pixels {
		if b != 0 {
			return true
		}
	}
	return false
}

// Both orientations must produce the wire buffer size (spec §3: the HID
// buffer is ALWAYS 320x170 = 108800 RGB565 bytes; portrait is pre-rotated).
func TestRenderFrameBytes(t *testing.T) {
	rd := sampleReadings()

	lt, err := config.ParseTheme([]byte(landscapeDraft))
	if err != nil {
		t.Fatal(err)
	}
	f, err := Render(&lt.Screens[0], lt, 320, 170, false, rd, NewFonts(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Pixels) != 320*170*2 {
		t.Fatalf("landscape pixels %d, want %d", len(f.Pixels), 320*170*2)
	}
	if !nonBlank(f) {
		t.Fatal("landscape frame is blank — text widget rendered nothing")
	}

	pt, err := config.ParseTheme([]byte(portraitDraft))
	if err != nil {
		t.Fatal(err)
	}
	f, err = Render(&pt.Screens[0], pt, 320, 170, true, rd, NewFonts(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Pixels) != 320*170*2 {
		t.Fatalf("portrait pixels %d, want %d (buffer is always 320x170)", len(f.Pixels), 320*170*2)
	}
	if !nonBlank(f) {
		t.Fatal("portrait frame is blank — pre-rotated text rendered nothing")
	}
}

func TestWritePNG(t *testing.T) {
	f := &Frame{Width: 2, Height: 1, Pixels: []byte{0xF8, 0x00, 0xF8, 0x00}}
	var buf bytes.Buffer
	if err := f.WritePNG(&buf); err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(&buf)
	if err != nil {
		t.Fatalf("written PNG does not decode: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 2 || b.Dy() != 1 {
		t.Fatalf("PNG size %dx%d, want 2x1", b.Dx(), b.Dy())
	}
	r, g, bl, _ := img.At(0, 0).RGBA()
	if r>>8 != 0xFF || g>>8 != 0x00 || bl>>8 != 0x00 {
		t.Fatalf("red channel round trip: %04X %04X %04X", r, g, bl)
	}
}
