package render

import (
	"image"
	"image/color"
	_ "image/jpeg" // the original image.js loaded PNG and JPEG files
	_ "image/png"  // register the PNG decoder for image.Decode
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fjodor-rybakov/mini-pc-panel/golang/internal/config"
	"github.com/fjodor-rybakov/mini-pc-panel/golang/internal/metrics"
	"github.com/tdewolff/canvas"
)

// drawWidget renders one theme widget into the current view (portrait themes
// arrive already pre-rotated by Render). Coordinates are theme-space pixels.
// The original wrapped every widget in beginPath/rect/clip (start_draw in
// each widget file); tdewolff/canvas has no clip, so chart geometry is built
// to stay inside the rect and the image widget crops its source instead.
// After drawing, debug_frame draws the original 1px red debug_rect.
func drawWidget(ctx *canvas.Context, w config.Widget, bg string, theme *config.Theme, rd map[string]metrics.Reading, fonts *Fonts) error {
	x, y := float64(w.Rect.X), float64(w.Rect.Y)
	ww, hh := float64(w.Rect.Width), float64(w.Rect.Height)
	if ww <= 0 || hh <= 0 {
		return nil
	}
	switch w.Type {
	case "", "none", "spacer":
		return nil
	case "text":
		if err := drawTextWidget(ctx, w, rd, fonts); err != nil {
			return err
		}
	case "doughnut":
		drawDoughnutChart(ctx, x, y, ww, hh, w, rd, bg, true)
	case "doughnut_chart":
		drawDoughnutChart(ctx, x, y, ww, hh, w, rd, bg, false)
	case "line_chart":
		drawLineChart(ctx, x, y, ww, hh, w, rd)
	case "bar_chart":
		drawBarChart(ctx, x, y, ww, hh, w, rd)
	case "custom_bar":
		drawCustomBar(ctx, x, y, ww, hh, w, rd)
	case "image":
		drawImageWidget(ctx, x, y, ww, hh, w, theme)
	default:
		return nil // config.Validate whitelists types; be forgiving anyway
	}
	if w.DebugFrame { // debug_rect() in every original widget file
		ctx.SetStroke(canvas.Hex("#ff0000"))
		ctx.SetStrokeWidth(1)
		strokeRect(ctx, x, y, ww, hh)
	}
	return nil
}

// ---- text ----

// drawTextWidget mirrors widgets/text.js: fallback font '20px Arial' and
// fillStyle 'white', textAlign left/center/right computed against the
// measured text width, and textBaseline TOP - the first line sits at the
// rect's top edge, not vertically centred.
func drawTextWidget(ctx *canvas.Context, w config.Widget, rd map[string]metrics.Reading, fonts *Fonts) error {
	ft, err := fonts.get(w.Font.Family, w.Font.Bold)
	if err != nil {
		return err
	}
	size := w.Font.Size
	if size <= 0 {
		size = 20 // text.js line 44 fallback '20px Arial'
	}
	col := w.Color
	if col == "" {
		col = "#ffffff" // text.js line 43 fallback 'white'
	}
	face := ft.Face(size, canvas.Hex(col))
	halign := canvas.Left
	switch w.Align {
	case "center":
		halign = canvas.Center
	case "right":
		halign = canvas.Right
	}
	text := canvas.NewTextBox(face, FormatTokens(w, rd), float64(w.Rect.Width), float64(w.Rect.Height), halign, canvas.Top, nil)
	ctx.DrawText(float64(w.Rect.X), float64(w.Rect.Y), text)
	return nil
}

// FormatTokens expands a widget format template: {0}..{9} reference the
// sensor Reading's pre-formatted Text tokens, {v} the integer primary value
// and {v1} one decimal place. An unsampled sensor renders as "N/A".
func FormatTokens(w config.Widget, rd map[string]metrics.Reading) string {
	if w.Format == "" && w.Value != "" {
		return w.Value // literal text widget
	}
	s, ok := rd[w.Sensor]
	if !ok {
		return "N/A"
	}
	if w.Format == "" {
		if len(s.Text) > 0 {
			return s.Text[0]
		}
		return strconv.FormatFloat(s.Value, 'f', 0, 64)
	}
	var b strings.Builder
	for i := 0; i < len(w.Format); i++ {
		if w.Format[i] != '{' {
			b.WriteByte(w.Format[i])
			continue
		}
		end := strings.IndexByte(w.Format[i:], '}')
		if end < 1 {
			b.WriteByte(w.Format[i])
			continue
		}
		b.WriteString(expandToken(w.Format[i+1:i+end], s))
		i += end
	}
	return b.String()
}

func expandToken(tok string, s metrics.Reading) string {
	switch tok {
	case "v":
		return strconv.FormatFloat(s.Value, 'f', 0, 64)
	case "v1":
		return strconv.FormatFloat(s.Value, 'f', 1, 64)
	}
	n, err := strconv.Atoi(tok)
	if err != nil || n < 0 {
		return ""
	}
	if n < len(s.Text) {
		return s.Text[n]
	}
	return ""
}

// ---- chart helpers ----

// chartSeries mirrors how main.js hands the CSV history to the chart
// widgets: data[<widget_config.value>]. A format token naming a join
// ({1} -> token 1's series) selects that series; otherwise the primary
// History (Reading.SeriesAt falls back to it).
func chartSeries(w config.Widget, s metrics.Reading) []float64 {
	for i := 0; i < len(w.Format); i++ {
		if w.Format[i] != '{' {
			continue
		}
		end := strings.IndexByte(w.Format[i:], '}')
		if end > 1 {
			if n, err := strconv.Atoi(w.Format[i+1 : i+end]); err == nil && n >= 0 {
				if ser := s.SeriesAt(n); len(ser.Values) > 0 {
					return ser.Values
				}
			}
		}
	}
	return s.History
}

// chartBounds mirrors the draw() convention in main.js lines 383-402
// (_min = sensor.min, _max = sensor.max, widget "max" overrides) plus the
// Chart.js autoscale when no bound is known: the y axis spans lo..hi with
// hi defaulting to the largest plotted point.
func chartBounds(w config.Widget, s metrics.Reading, pts []float64) (float64, float64) {
	lo := s.Min
	hi := w.Max
	if hi <= 0 {
		hi = s.Max
	}
	if hi <= lo {
		for _, v := range pts {
			if v > hi {
				hi = v
			}
		}
	}
	if hi <= lo {
		hi = lo + 1
	}
	return lo, hi
}

// ---- doughnut charts ----

// drawDoughnutChart mirrors doughnut_chart.js: a Chart.js doughnut with
// data [value-min, max-value], colours config.used (#48BB78 default) and
// config.free (#EDF2F7 default, also the borderColor), rotation 225,
// circumference 270 and cutout 80%. The plain legacy "doughnut" type uses
// the same ring renderer with a full circle and Thickness as ring width.
// Chart.js rendered the ring over transparency, so the hole is a real hole
// (wallpaper/below widgets stay visible), not a background-coloured disc.
func drawDoughnutChart(ctx *canvas.Context, x, y, ww, hh float64, w config.Widget, rd map[string]metrics.Reading, bg string, plain bool) {
	s := rd[w.Sensor]
	lo, hi := chartBounds(w, s, nil)
	frac := clamp01((s.Value - lo) / (hi - lo))

	used := w.Used
	if used == "" {
		used = w.Color
	}
	if used == "" {
		if plain {
			used = "#00ff00"
		} else {
			used = "#48BB78" // doughnut_chart.js backgroundColor[0]
		}
	}
	free := w.Free
	if free == "" {
		if plain {
			free = bg
		} else {
			free = "#EDF2F7" // doughnut_chart.js backgroundColor[1]/borderColor
		}
	}

	rot, circ, cut := float64(w.Rotation), float64(w.Circumference), float64(w.Cutout)
	if plain {
		if circ == 0 {
			circ = 360
		}
	} else {
		if rot == 0 {
			rot = 225 // doughnut_chart.js rotation default
		}
		if circ == 0 {
			circ = 270 // doughnut_chart.js circumference default
		}
		if cut == 0 {
			cut = 80 // doughnut_chart.js cutout '80%' default
		}
	}
	cx, cy := x+ww/2, y+hh/2
	rOut := min(ww, hh) / 2
	if plain {
		// legacy Go ring: Thickness is the ring width in pixels
		thick := float64(w.Thickness)
		if thick <= 0 || thick >= rOut {
			thick = rOut / 3
		}
		cut = (1 - thick/rOut) * 100
	}
	cut = min(max(cut, 0), 99)
	rIn := rOut * cut / 100

	// Chart.js rotation/circumference are degrees clockwise from 12 o'clock.
	// In the CartesianIV view (y down) a y-down math angle rotated by -90
	// sweeps visually clockwise as it increases, so CW-from-top = math+90.
	a0 := rot - 90
	annularSector(ctx, cx, cy, rOut, rIn, a0, a0+circ, free)
	if frac > 0 {
		annularSector(ctx, cx, cy, rOut, rIn, a0, a0+circ*frac, used)
	}
}

// annularSector fills the ring sector between y-down math angles a0..a1
// (degrees; increasing sweeps visually clockwise on the panel). Chart.js
// produced this from its own canvas; tdewolff/canvas has no annulus
// primitive and Arc cannot reliably sweep the inner edge backwards, so
// both arcs are 6-degree stepped polylines (chord error < 0.25 px at the
// panel's radii).
func annularSector(ctx *canvas.Context, cx, cy, rOut, rIn, a0, a1 float64, col string) {
	if rOut <= 0 || a1 <= a0 {
		return
	}
	if rIn < 0 {
		rIn = 0
	}
	const step = 6
	arc := func(r, a float64) (float64, float64) {
		ra := a * math.Pi / 180
		return cx + r*math.Cos(ra), cy + r*math.Sin(ra)
	}
	ctx.SetFill(canvas.Hex(col))
	sx, sy := arc(rOut, a0)
	ctx.MoveTo(sx, sy)
	for a := a0 + step; a < a1; a += step {
		px, py := arc(rOut, a)
		ctx.LineTo(px, py)
	}
	ex, ey := arc(rOut, a1)
	ctx.LineTo(ex, ey)
	ix, iy := arc(rIn, a1)
	ctx.LineTo(ix, iy)
	for a := a1 - step; a > a0; a -= step {
		px, py := arc(rIn, a)
		ctx.LineTo(px, py)
	}
	jx, jy := arc(rIn, a0)
	ctx.LineTo(jx, jy)
	ctx.Close()
	ctx.Fill()
}

// ---- line chart ----

// drawLineChart mirrors line_chart.js (Chart.js line chart): the stroke is
// config.outline (#4d4d4d default) and the fill area - only when
// config.area is true - is config.fill (#00e600 default), NOT the widget
// text colour; borderWidth is config.thickness (Chart.js default 3). The
// 0.1 tension is approximated with straight segments (documented
// deviation).
func drawLineChart(ctx *canvas.Context, x, y, ww, hh float64, w config.Widget, rd map[string]metrics.Reading) {
	s := rd[w.Sensor]
	all := chartSeries(w, s)
	if len(all) < 2 {
		return
	}
	n := w.Points
	if n <= 0 {
		n = 30
	}
	n = min(n, len(all))
	pts := all[len(all)-n:]
	lo, hi := chartBounds(w, s, pts)
	if hi <= lo {
		return
	}
	stroke := w.Outline
	if stroke == "" {
		stroke = "#4d4d4d" // line_chart.js borderColor default
	}
	area := w.Fill
	if area == "" {
		area = "#00e600" // line_chart.js backgroundColor default
	}
	thick := w.Thickness
	if thick <= 0 {
		thick = 3 // Chart.js default borderWidth
	}
	// x scale: linear, min 0 max points.length (Chart.js axes in the
	// original files); point i sits at i/n of the width.
	dx := ww / float64(n)
	py := func(v float64) float64 { return y + hh - clamp01((v-lo)/(hi-lo))*hh }

	if w.Area {
		ctx.SetFill(canvas.Hex(area))
		ctx.MoveTo(x, y+hh)
		for i, v := range pts {
			ctx.LineTo(x+float64(i)*dx, py(v))
		}
		ctx.LineTo(x+float64(n-1)*dx, y+hh)
		ctx.Close()
		ctx.Fill()
	}
	ctx.SetStroke(canvas.Hex(stroke))
	ctx.SetStrokeWidth(thick)
	ctx.MoveTo(x, py(pts[0]))
	for i, v := range pts[1:] {
		ctx.LineTo(x+float64(i+1)*dx, py(v))
	}
	ctx.Stroke()
}

// ---- bar chart ----

// drawBarChart mirrors bar_chart.js (Chart.js bar chart): one bar per
// history point, config.thickness px wide (barThickness default 1) with a
// 1 px config.outline border and config.fill fill (#00e600 default).
// horizontal maps to indexAxis 'y'. The config.zoom drawImage stretch
// trick is not reproduced (documented deviation).
func drawBarChart(ctx *canvas.Context, x, y, ww, hh float64, w config.Widget, rd map[string]metrics.Reading) {
	s := rd[w.Sensor]
	all := chartSeries(w, s)
	if len(all) == 0 {
		return
	}
	n := w.Points
	if n <= 0 {
		n = 30
	}
	n = min(n, len(all))
	pts := all[len(all)-n:]
	lo, hi := chartBounds(w, s, pts)
	if hi <= lo {
		return
	}
	fill := w.Fill
	if fill == "" {
		fill = "#00e600" // bar_chart.js backgroundColor default
	}
	border := w.Outline
	if border == "" {
		border = "#4d4d4d" // bar_chart.js borderColor default
	}
	thick := w.Thickness
	if thick <= 0 {
		thick = 1 // bar_chart.js barThickness default
	}
	ctx.SetStroke(canvas.Hex(border))
	ctx.SetStrokeWidth(1)
	for i, v := range pts {
		frac := clamp01((v - lo) / (hi - lo))
		if w.Horizontal {
			c := y + float64(i)/float64(n)*hh
			ctx.SetFill(canvas.Hex(fill))
			fillRect(ctx, x, c-thick/2, ww*frac, thick)
			strokeRect(ctx, x, c-thick/2, ww*frac, thick)
			continue
		}
		c := x + float64(i)/float64(n)*ww
		bh := frac * hh
		ctx.SetFill(canvas.Hex(fill))
		fillRect(ctx, c-thick/2, y+hh-bh, thick, bh)
		strokeRect(ctx, c-thick/2, y+hh-bh, thick, bh)
	}
}

// ---- custom bar ----

// drawCustomBar mirrors custom_bar.js: the value is a 0..100 percentage;
// the full rect gets a vertical gradient #404040 -> #000000, the filled
// part (width = rect.width * value/100) a vertical gradient lightgreen
// (#90EE90) -> darkgreen (#006400).
func drawCustomBar(ctx *canvas.Context, x, y, ww, hh float64, w config.Widget, rd map[string]metrics.Reading) {
	s := rd[w.Sensor]
	ctx.SetFill(vGradient(y, hh, "#404040", "#000000"))
	fillRect(ctx, x, y, ww, hh)
	ctx.SetFill(vGradient(y, hh, "#90EE90", "#006400"))
	fillRect(ctx, x, y, ww*clamp01(s.Value/100), hh)
}

// vGradient builds the vertical linear gradient the original
// ctx.createLinearGradient(x, y, x, y+height) produced.
func vGradient(y, hh float64, from, to string) *canvas.LinearGradient {
	g := canvas.NewGradient()
	g.Add(0, rgbOf(from))
	g.Add(1, rgbOf(to))
	return g.ToLinear(canvas.Point{X: 0, Y: y}, canvas.Point{X: 0, Y: y + hh})
}

func rgbOf(s string) color.RGBA {
	r, g, b, a := canvas.Hex(s).RGBA()
	return color.RGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: uint8(a >> 8)}
}

// ---- image widget and wallpaper ----

type imageEntry struct {
	mtime time.Time
	img   image.Image
}

// imageCache mirrors the image.js _private.image: a decoded image stays
// cached per path; the mtime check reproduces the reload-on-change.
var (
	imageCacheMu sync.Mutex
	imageCache   = map[string]imageEntry{}
)

// loadImageFile decodes a PNG/JPEG with an mtime-checked cache.
func loadImageFile(path string) (image.Image, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	imageCacheMu.Lock()
	defer imageCacheMu.Unlock()
	if e, ok := imageCache[path]; ok && e.mtime.Equal(st.ModTime()) {
		return e.img, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return nil, err
	}
	imageCache[path] = imageEntry{st.ModTime(), img}
	return img, nil
}

// resolveThemePath maps wallpaper/image paths relative to the theme file
// directory (the original resolved against the process cwd; theme-relative
// also finds the shipped themes' assets). ParseTheme drafts have no Dir,
// so those keep the cwd semantics.
func resolveThemePath(theme *config.Theme, name string) string {
	if theme != nil && theme.Dir != "" && !filepath.IsAbs(name) {
		return filepath.Join(theme.Dir, name)
	}
	return name
}

// drawImageWidget mirrors image.js: context.drawImage(image, rect.x,
// rect.y) at native size, clipped to the widget rect. Without a canvas
// clip the clip is emulated by cropping the source (drawImage places the
// source's top-left corner at the rect origin).
func drawImageWidget(ctx *canvas.Context, x, y, ww, hh float64, w config.Widget, theme *config.Theme) {
	name := w.Value
	if name == "" {
		return
	}
	img, err := loadImageFile(resolveThemePath(theme, name))
	if err != nil {
		return // the original only console.log'd; the panel shows the background
	}
	ctx.DrawImage(x, y, cropToSize(img, int(ww), int(hh)), canvas.DPMM(1.0))
}

// cropToSize keeps the top-left w x h part of an image.
func cropToSize(img image.Image, w, h int) image.Image {
	b := img.Bounds()
	if w <= 0 || h <= 0 {
		return image.NewRGBA(image.Rectangle{})
	}
	cw, ch := min(b.Dx(), w), min(b.Dy(), h)
	if cw == b.Dx() && ch == b.Dy() {
		return img
	}
	out := image.NewRGBA(image.Rect(0, 0, cw, ch))
	for py := 0; py < ch; py++ {
		for px := 0; px < cw; px++ {
			out.Set(px, py, img.At(b.Min.X+px, b.Min.Y+py))
		}
	}
	return out
}

// drawWallpaper mirrors the original screen wallpaper draw: the picture is
// scaled to cover the whole theme canvas (centred; the overflow lands
// outside the page and is clipped by the rasteriser).
func drawWallpaper(ctx *canvas.Context, theme *config.Theme, name string, w, h float64, portrait bool) {
	img, err := loadImageFile(resolveThemePath(theme, name))
	if err != nil {
		return
	}
	cw, ch := w, h
	if portrait {
		cw, ch = h, w // drawn inside the pre-rotated view: theme space dims
	}
	b := img.Bounds()
	iw, ih := float64(b.Dx()), float64(b.Dy())
	if iw <= 0 || ih <= 0 {
		return
	}
	scale := max(cw/iw, ch/ih)
	ctx.DrawImage((cw-iw*scale)/2, (ch-ih*scale)/2, img, canvas.DPMM(1.0/scale))
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
