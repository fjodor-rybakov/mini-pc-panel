// Package render rasterises theme screens into the panel's RGB565
// framebuffer (doc/revrite.md §3).
package render

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"

	"github.com/fjodor-rybakov/mini-pc-panel/internal/config"
	"github.com/fjodor-rybakov/mini-pc-panel/internal/metrics"
	"github.com/tdewolff/canvas"
	"github.com/tdewolff/canvas/renderers/rasterizer"
)

// Frame is a rasterised screen ready for the panel: RGB565 pixels in
// big-endian byte order (spec §3), row-major, len == Width*Height*2.
type Frame struct {
	Width  int
	Height int
	Pixels []byte
}

// Render draws one screen of the theme into a w x h landscape framebuffer
// (normally 320x170). In portrait themes the widget coordinates live in the
// 170x320 portrait space; drawing is pre-rotated into the landscape buffer
// exactly like the original Node implementation (translate(0,170) +
// rotate(-90deg)), while the device performs the physical rotation itself
// via set_orientation.
//
// canvas coordinates equal device pixels at canvas.DPMM(1.0): the canvas is
// created w x h "millimetres" and rasterised at 1 dot/mm.
func Render(sc *config.Screen, theme *config.Theme, w, h int, portrait bool, rd map[string]metrics.Reading, fonts *Fonts) (*Frame, error) {
	if sc == nil {
		return nil, fmt.Errorf("render: nil screen")
	}
	if rd == nil {
		rd = map[string]metrics.Reading{}
	}
	c := canvas.New(float64(w), float64(h))
	ctx := canvas.NewContext(c)
	// Top-left origin to match theme rects (spec §3 axis note neutralised).
	ctx.SetCoordSystem(canvas.CartesianIV)

	bg := theme.Background
	if sc.Background != "" {
		bg = sc.Background
	}
	if bg == "" {
		bg = "#000000"
	}
	ctx.SetFill(canvas.Hex(bg))
	fillRect(ctx, 0, 0, float64(w), float64(h))

	ctx.Push()
	if portrait {
		// (u,v) portrait space -> (v, h-u) landscape buffer space; the
		// matching device-space rect transform lives in app.appDeviceRect
		// (same math as translate_rect in the original main.js).
		ctx.ComposeView(canvas.Matrix{{0, 1, 0}, {-1, 0, float64(h)}})
	}
	if sc.Wallpaper != "" {
		// screen wallpaper: scaled to cover the theme canvas, centred,
		// drawn before the widgets (as in the original renderer).
		drawWallpaper(ctx, theme, sc.Wallpaper, float64(w), float64(h), portrait)
	}
	for _, wid := range sc.Widgets {
		if err := drawWidget(ctx, wid, bg, theme, rd, fonts); err != nil {
			return nil, fmt.Errorf("render: screen %d widget %d (%s): %w", sc.ID, wid.ID, wid.Type, err)
		}
	}
	ctx.Pop()

	img := rasterizer.Draw(c, canvas.DPMM(1.0), canvas.DefaultColorSpace)
	return &Frame{Width: w, Height: h, Pixels: RGB565FromRGBA(img)}, nil
}

// RGB565FromRGBA converts an opaque RGBA image to the panel's big-endian
// RGB565 stream (spec §3): rgb565 = (r>>3)<<11 | (g>>2)<<5 | (b>>3), high
// byte written first.
func RGB565FromRGBA(img *image.RGBA) []byte {
	n := img.Rect.Dx() * img.Rect.Dy()
	out := make([]byte, n*2)
	j := 0
	for y := img.Rect.Min.Y; y < img.Rect.Max.Y; y++ {
		for x := img.Rect.Min.X; x < img.Rect.Max.X; x++ {
			off := img.PixOffset(x, y)
			r := uint16(img.Pix[off]) >> 3
			g := uint16(img.Pix[off+1]) >> 2
			b := uint16(img.Pix[off+2]) >> 3
			v := (r << 11) | (g << 5) | b
			out[j] = byte(v >> 8)
			out[j+1] = byte(v & 0xFF)
			j += 2
		}
	}
	return out
}

// Region extracts a row-major RGB565 blob for the rectangle (x,y,w,h); the
// app hands it straight to display.Update.
func (f *Frame) Region(x, y, w, h int) []byte {
	out := make([]byte, w*h*2)
	for row := 0; row < h; row++ {
		src := ((y+row)*f.Width + x) * 2
		copy(out[row*w*2:(row+1)*w*2], f.Pixels[src:src+w*2])
	}
	return out
}

// SameRegion reports whether two frames hold identical pixels in the rect.
func (f *Frame) SameRegion(other *Frame, x, y, w, h int) bool {
	if other == nil || other.Width != f.Width || other.Height != f.Height {
		return false
	}
	for row := 0; row < h; row++ {
		o := ((y+row)*f.Width + x) * 2
		if !bytes.Equal(f.Pixels[o:o+w*2], other.Pixels[o:o+w*2]) {
			return false
		}
	}
	return true
}

// WritePNG encodes the frame as PNG (editor previews in the browser).
func (f *Frame) WritePNG(wr io.Writer) error {
	img := image.NewRGBA(image.Rect(0, 0, f.Width, f.Height))
	j := 0
	for y := 0; y < f.Height; y++ {
		for x := 0; x < f.Width; x++ {
			v := uint16(f.Pixels[j])<<8 | uint16(f.Pixels[j+1])
			j += 2
			r5 := v >> 11
			g6 := (v >> 5) & 0x3F
			b5 := v & 0x1F
			img.SetRGBA(x, y, color.RGBA{
				R: uint8(r5<<3 | r5>>2),
				G: uint8(g6<<2 | g6>>4),
				B: uint8(b5<<3 | b5>>2),
				A: 0xFF,
			})
		}
	}
	return png.Encode(wr, img)
}

// fillRect draws an axis-aligned filled rectangle in the current view
// (tdewolff/canvas has no Rectangle primitive in this version).
func fillRect(ctx *canvas.Context, x, y, w, h float64) {
	if w <= 0 || h <= 0 {
		return
	}
	ctx.MoveTo(x, y)
	ctx.LineTo(x+w, y)
	ctx.LineTo(x+w, y+h)
	ctx.LineTo(x, y+h)
	ctx.Close()
	ctx.Fill()
}

// strokeRect draws an axis-aligned rectangle outline.
func strokeRect(ctx *canvas.Context, x, y, w, h float64) {
	if w <= 0 || h <= 0 {
		return
	}
	ctx.MoveTo(x, y)
	ctx.LineTo(x+w, y)
	ctx.LineTo(x+w, y+h)
	ctx.LineTo(x, y+h)
	ctx.Close()
	ctx.Stroke()
}
