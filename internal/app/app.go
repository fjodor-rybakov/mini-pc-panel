// Package app wires config, HID panel, LED, metrics and rendering into the
// daemon lifecycle described in doc/revrite.md §5.
package app

import (
	"context"
	"fmt"
	"log"
	"maps"
	"sync"
	"time"

	"github.com/fjodor-rybakov/mini-pc-panel/internal/config"
	"github.com/fjodor-rybakov/mini-pc-panel/internal/display"
	"github.com/fjodor-rybakov/mini-pc-panel/internal/led"
	"github.com/fjodor-rybakov/mini-pc-panel/internal/metrics"
	"github.com/fjodor-rybakov/mini-pc-panel/internal/render"
)

// previewHold keeps an editor preview visible on the real panel long enough
// to be seen before the live loop repaints (spec has no preview concept; the
// original web UI had the same "apply to screen" action).
const previewHold = 5 * time.Second

// App owns all subsystems and the panel state machine.
type App struct {
	cfg      *config.Config
	registry *metrics.Registry
	fonts    *render.Fonts
	disp     *display.Display
	led      *led.Device // nil when disabled or the port is missing

	mu           sync.Mutex
	theme        *config.Theme
	themeDirty   bool
	screenIdx    int
	screenSince  time.Time
	screenPaused bool // original /api/pause?on=
	forceRedraw  bool
	// lastWriteFails remembers display.WriteFailures() as of the previous
	// tick; only the Run/tick goroutine touches it, so it needs no lock.
	lastWriteFails uint64
	lastFrame      *render.Frame
	readings       map[string]metrics.Reading
	previewUntil   time.Time
	previewLED     bool // preview pushed the draft screen's LED; restore after it expires
	lastHeartbeat  time.Time
	manualLED      *config.LEDSettings // editor-driven LED set (outranks config)
	lastLEDTheme   int                 // mode the strip now holds (0 = nothing sent)
}

// New opens the hardware and loads the theme; it does not start traffic yet.
// A missing LED port is a soft failure (the panel still works), matching the
// original led_thread.js tolerance.
func New(cfg *config.Config) (*App, error) {
	var tr display.Transport
	var err error
	if cfg.Device.Path != "" {
		tr, err = display.OpenPath(cfg.Device.Path)
	} else {
		tr, err = display.OpenHID(uint16(cfg.Device.VID), uint16(cfg.Device.PID), cfg.Device.Interface)
	}
	if err != nil {
		return nil, err
	}
	theme, err := config.LoadTheme(cfg.ThemeFile)
	if err != nil {
		tr.Close()
		return nil, err
	}
	a := &App{
		cfg:         cfg,
		registry:    BuildRegistry(cfg),
		fonts:       render.NewFonts(cfg.Fonts),
		disp:        display.New(tr, time.Duration(cfg.PacketDelay)*time.Millisecond, time.Duration(cfg.RedrawDelay)*time.Millisecond),
		theme:       theme,
		forceRedraw: true,
	}
	if cfg.LED.Enabled == nil || *cfg.LED.Enabled {
		dev, err := led.Open(cfg.LED.Port)
		if err != nil {
			log.Printf("led: %v (continuing without LED control)", err)
		} else {
			a.led = dev
		}
	}
	a.warnUnknownSensors(theme)
	return a, nil
}

// BuildRegistry creates the sensor set selected by the config. Sensor names
// match the original modules (clock, calendar, cpu_usage, memory,
// space_<mount>, network_<iface>..., cpu_temp, cpu_power).
func BuildRegistry(cfg *config.Config) *metrics.Registry {
	maxPoints := cfg.Sensors.MaxPoints
	s := []metrics.Sensor{
		metrics.Clock(),
		metrics.Calendar(),
		metrics.CPUUsage(maxPoints),
		metrics.Memory(maxPoints),
	}
	for _, m := range cfg.Sensors.MountPoints {
		s = append(s, metrics.Space(m, maxPoints))
	}
	for _, iface := range cfg.Sensors.NetworkInterfaces {
		s = append(s, metrics.Network(iface, maxPoints, cfg.Sensors.Scaling, ""))
	}
	s = append(s, metrics.HwmonTemp("/sys/class/hwmon", metrics.TempOptions{
		Chips:      cfg.Sensors.HwmonChips,
		Labels:     cfg.Sensors.HwmonTempLabels,
		Fahrenheit: cfg.Sensors.Fahrenheit,
		MaxPoints:  maxPoints,
	}))
	s = append(s, metrics.CPUPower("/sys/class/powercap", "/sys/class/hwmon",
		cfg.Sensors.HwmonChips, cfg.Sensors.HwmonPowerLabels, maxPoints))
	return metrics.NewRegistry(s...)
}

// Run performs the §5.2 lifecycle and blocks until ctx is cancelled.
func (a *App) Run(ctx context.Context) error {
	a.disp.Start(ctx)
	defer func() {
		a.shutdownLED()
		a.led.Close()
		a.disp.Close()
	}()

	// START_COOL_DOWN in main.js: 1000 ms between opening the HID device
	// and the first command so the descriptors settle.
	select {
	case <-time.After(time.Second):
	case <-ctx.Done():
		return nil
	}

	a.applyThemeOrientation(ctx)
	if err := a.disp.SetTime(ctx, time.Now()); err != nil {
		return fmt.Errorf("app: set time: %w", err)
	}

	// first full frame (startup sequence: orientation -> time -> redraw)
	a.refresh()
	frame, err := a.renderCurrent()
	if err != nil {
		return err
	}
	if err := a.disp.Redraw(ctx, frame.Pixels); err != nil {
		return fmt.Errorf("app: initial redraw: %w", err)
	}
	a.setLastFrame(frame)
	a.applyScreenLED(a.currentScreen())
	a.markHeartbeatSent()

	// The original lcd_thread.js does NOT heartbeat on a fixed timer: it
	// checks the conditions about once per second (see maybeHeartbeat).
	hbCheck := time.NewTicker(time.Second)
	defer hbCheck.Stop()
	poll := time.NewTicker(time.Duration(a.cfg.Poll) * time.Millisecond)
	defer poll.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("app: shutdown signal received")
			return nil
		case <-hbCheck.C:
			a.maybeHeartbeat(ctx)
		case <-poll.C:
			if err := a.tick(ctx); err != nil {
				log.Printf("app: tick: %v", err)
			}
		}
	}
}

// maybeHeartbeat implements the original heartbeat rule (lcd_thread.js):
// send when the queue is busy AND more than cfg.Heartbeat ms passed since
// the last beat, OR when the queue has been idle for at least
// cfg.RedrawDelay (config.refresh) ms - the panel needs traffic even while
// nothing changes. Spec §5.2: never more often than once per second.
func (a *App) maybeHeartbeat(ctx context.Context) {
	hbEvery := time.Duration(a.cfg.Heartbeat) * time.Millisecond
	if hbEvery <= 0 {
		hbEvery = time.Minute
	}
	refresh := time.Duration(a.cfg.RedrawDelay) * time.Millisecond
	pending := a.disp.Pending()
	idle := a.disp.IdleFor()

	a.mu.Lock()
	since := time.Since(a.lastHeartbeat)
	a.mu.Unlock()
	if since < time.Second {
		return
	}
	if !((pending > 0 && since >= hbEvery) || (pending == 0 && idle >= refresh)) {
		return
	}
	if err := a.disp.Heartbeat(ctx, time.Now()); err != nil {
		log.Printf("app: heartbeat: %v", err)
		return
	}
	a.markHeartbeatSent()
}

func (a *App) markHeartbeatSent() {
	a.mu.Lock()
	a.lastHeartbeat = time.Now()
	a.mu.Unlock()
}

// tick rotates screens, samples metrics and sends only what changed.
func (a *App) tick(ctx context.Context) error {
	// display.process drops the rest of a job when a Write fails, so the
	// panel no longer matches lastFrame (our diff baseline). Any change in
	// the failure counter means "assume the worst": latch a full redraw.
	if fails := a.disp.WriteFailures(); fails != a.lastWriteFails {
		a.lastWriteFails = fails
		a.mu.Lock()
		a.forceRedraw = true
		a.mu.Unlock()
	}
	a.mu.Lock()
	previewing := time.Now().Before(a.previewUntil)
	dirty := a.themeDirty
	expiredPreview := !previewing && a.previewLED
	if expiredPreview {
		a.previewLED = false
	}
	a.mu.Unlock()

	if dirty {
		a.applyThemeOrientation(ctx)
		a.mu.Lock()
		a.screenIdx = a.firstEnabledIndex()
		a.screenSince = time.Now()
		a.forceRedraw = true
		a.themeDirty = false
		a.mu.Unlock()
		a.applyScreenLED(a.currentScreen())
	}

	a.refresh()
	if previewing {
		return nil // editor preview still on screen; metrics kept sampling
	}
	// Rotation must NOT run while the draft holds the screen (it used to
	// advance screens and push that screen's LED mid-preview). Once the
	// preview expires, put the strip back to the live screen's settings.
	if expiredPreview {
		a.applyScreenLED(a.currentScreen())
	}
	a.rotateScreen()

	frame, err := a.renderCurrent()
	if err != nil {
		return err
	}

	a.mu.Lock()
	last := a.lastFrame
	full := a.forceRedraw
	themeRefresh := a.theme.Refresh == "redraw"
	a.mu.Unlock()

	if last == nil || full || themeRefresh {
		if err := a.disp.Redraw(ctx, frame.Pixels); err != nil {
			return err
		}
		a.setLastFrame(frame)
		a.mu.Lock()
		a.forceRedraw = false
		a.mu.Unlock()
		return nil
	}

	// partial refresh: compare each widget's device-space region
	sc := a.currentScreen()
	_, _, portrait := a.Theme().OrientationSize()
	for _, w := range sc.Widgets {
		dr := clampToFrame(DeviceRect(w.Rect, portrait), frame.Width, frame.Height)
		if dr.Width <= 0 || dr.Height <= 0 {
			continue
		}
		if last.SameRegion(frame, dr.X, dr.Y, dr.Width, dr.Height) {
			continue
		}
		for _, sub := range display.SplitRect(display.Rect{X: dr.X, Y: dr.Y, Width: dr.Width, Height: dr.Height}) {
			if err := a.disp.Update(ctx, sub, frame.Region(sub.X, sub.Y, sub.Width, sub.Height)); err != nil {
				// Earlier sub-rects may already be on the panel: the screen
				// is now a mix of old and new frames. Don't adopt this frame
				// as the baseline - resync with a full redraw next tick.
				a.mu.Lock()
				a.forceRedraw = true
				a.mu.Unlock()
				return err
			}
		}
	}
	a.setLastFrame(frame)
	return nil
}

// rotateScreen advances to the next enabled screen once the screen duration
// expired. main.js line 271: duration 0/absent NEVER rotates; a positive
// duration is used as Math.max(duration, 10000) ms.
func (a *App) rotateScreen() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.screenPaused {
		return
	}
	screens := a.theme.Screens
	if len(screens) < 2 {
		return
	}
	if a.screenIdx < 0 || a.screenIdx >= len(screens) {
		// A hot theme swap can land between this tick's dirty check and
		// here: the stale index may be out of range for the new theme.
		a.screenIdx = a.firstEnabledIndex()
	}
	dur := screens[a.screenIdx].Duration
	if dur == 0 {
		return
	}
	if dur < 10000 {
		dur = 10000
	}
	if time.Since(a.screenSince) < time.Duration(dur)*time.Millisecond {
		return
	}
	a.advanceScreenLocked()
}

// advanceScreenLocked moves to the next enabled screen, forces a full
// redraw and applies that screen's LED settings. Callers hold a.mu.
func (a *App) advanceScreenLocked() {
	screens := a.theme.Screens
	next := a.screenIdx
	for i := 1; i <= len(screens); i++ {
		cand := (a.screenIdx + i) % len(screens)
		if screens[cand].IsEnabled() {
			next = cand
			break
		}
	}
	if next != a.screenIdx {
		a.screenIdx = next
		a.screenSince = time.Now()
		a.forceRedraw = true
		log.Printf("app: screen rotated to %d (%s)", next, screens[next].Name)
		a.applyScreenLEDLocked(&screens[next])
	}
}

// NextScreen advances rotation immediately (original /api/next_screen).
func (a *App) NextScreen() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.theme.Screens) < 2 {
		return
	}
	a.advanceScreenLocked()
}

// ShowScreen jumps straight to the enabled screen with the given theme id
// (the original /api/next_screen callback accepts a request.id and selects
// that screen instead of the next one). Returns false when no enabled
// screen carries that id.
func (a *App) ShowScreen(id int) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	screens := a.theme.Screens
	for i := range screens {
		if screens[i].ID != id || !screens[i].IsEnabled() {
			continue
		}
		if i != a.screenIdx {
			a.screenIdx = i
			a.screenSince = time.Now()
			a.forceRedraw = true
			log.Printf("app: screen switched to %d (%s)", i, screens[i].Name)
			a.applyScreenLEDLocked(&screens[i])
		}
		return true
	}
	return false
}

// PauseRotation freezes/unfreezes automatic rotation (original
// /api/pause?on=true).
func (a *App) PauseRotation(on bool) {
	a.mu.Lock()
	a.screenPaused = on
	a.mu.Unlock()
}

func (a *App) firstEnabledIndex() int {
	for i, sc := range a.theme.Screens {
		if sc.IsEnabled() {
			return i
		}
	}
	return 0
}

func (a *App) currentScreen() *config.Screen {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.screenIdx < 0 || a.screenIdx >= len(a.theme.Screens) {
		a.screenIdx = 0
	}
	return &a.theme.Screens[a.screenIdx]
}

func (a *App) renderCurrent() (*render.Frame, error) {
	a.mu.Lock()
	if a.screenIdx < 0 || a.screenIdx >= len(a.theme.Screens) {
		a.screenIdx = 0 // same clamp as currentScreen: a swap can beat the tick
	}
	sc := &a.theme.Screens[a.screenIdx]
	theme := a.theme
	a.mu.Unlock()
	_, _, portrait := theme.OrientationSize()
	return render.Render(sc, theme, display.FrameWidth, display.FrameHeight, portrait, a.LatestReadings(), a.fonts)
}

func (a *App) refresh() {
	a.mu.Lock()
	a.readings = a.registry.SampleAll()
	a.mu.Unlock()
}

func (a *App) setLastFrame(f *render.Frame) {
	a.mu.Lock()
	a.lastFrame = f
	a.mu.Unlock()
}

func (a *App) applyThemeOrientation(ctx context.Context) {
	_, _, portrait := a.Theme().OrientationSize()
	o := byte(display.OrientationLandscape)
	if portrait {
		o = display.OrientationPortrait
	}
	if err := a.disp.SetOrientation(ctx, o); err != nil {
		log.Printf("app: set orientation: %v", err)
	}
}

// ---- LED ----

func (a *App) applyScreenLED(sc *config.Screen) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.applyScreenLEDLocked(sc)
}

// applyScreenLEDLocked merges the per-screen led_config over the global LED
// settings field by field (zero = inherit). The original fell back to
// theme||4 on screen change but led_thread.js re-applied the global
// settings every 3 s; there is no such loop here, so inheriting the global
// defaults (config defaults to theme 5/3/3) is the documented deviation.
// The global side of that merge starts from the editor's manual settings
// when present (manualLED): without that, an "off" pushed from the editor
// was undone by the very next screen rotation, which re-applied the config
// theme. Callers hold a.mu (rotateScreen) or run single-threaded before
// the loop.
func (a *App) applyScreenLEDLocked(sc *config.Screen) {
	if a.led == nil || sc == nil {
		return
	}
	s := a.cfg.LED.LEDSettings
	if a.manualLED != nil {
		s = *a.manualLED
	}
	if sc.LED != nil {
		if sc.LED.Theme != 0 {
			s.Theme = sc.LED.Theme
		}
		if sc.LED.Intensity != 0 {
			s.Intensity = sc.LED.Intensity
		}
		if sc.LED.Speed != 0 {
			s.Speed = sc.LED.Speed
		}
	}
	switch s.Theme {
	case led.ThemeIgnore: // led_thread.js case 6: no packet at all
		log.Printf("app: led theme 6 (ignore): strip left as-is")
	case led.ThemeOff:
		if err := a.led.Off(); err != nil {
			log.Printf("app: led off: %v", err)
		} else {
			a.lastLEDTheme = s.Theme
		}
	default:
		if err := a.led.Set(s.Theme, s.Intensity, s.Speed); err != nil {
			log.Printf("app: led set: %v", err)
		} else {
			a.lastLEDTheme = s.Theme
		}
	}
}

// ApplyLEDManual pushes explicit strip settings (editor "apply LED" action).
// Config-scale intensity/speed; theme 6 sends nothing (original ignore).
// The settings also become the live global for every later screen change,
// so an editor "off" is no longer undone by the next rotation. They last
// for the process lifetime only: the daemon never writes config.json, so a
// restart falls back to its led block.
func (a *App) ApplyLEDManual(settings config.LEDSettings) error {
	if a.led == nil {
		return fmt.Errorf("app: led device not open")
	}
	if settings.Theme < led.ThemeRainbow || settings.Theme > led.ThemeIgnore {
		return fmt.Errorf("app: led: theme must be 1..6, got %d", settings.Theme)
	}
	// Port writes run under a.mu as well: the rotation path
	// (applyScreenLEDLocked) drives the same CH340 from the loop
	// goroutine, and interleaved FA..checksum frames latch garbage.
	a.mu.Lock()
	defer a.mu.Unlock()
	switch settings.Theme {
	case led.ThemeIgnore:
		log.Printf("app: led theme 6 (ignore): no packet sent")
	case led.ThemeOff:
		if err := a.led.Off(); err != nil {
			return err
		}
	default:
		if err := a.led.Set(settings.Theme, settings.Intensity, settings.Speed); err != nil {
			return err
		}
	}
	if settings.Theme != led.ThemeIgnore {
		a.lastLEDTheme = settings.Theme
	}
	m := settings
	a.manualLED = &m
	return nil
}

// shutdownLED turns the strip off when the daemon exits while it had been
// driving it: the CH340 controller latches its last mode, so a stopped (or
// afterwards led.enabled=false) daemon would otherwise leave the strip
// burning. When the strip was never touched, or left alone via theme 6
// ("ignore"), nothing is sent.
func (a *App) shutdownLED() {
	a.mu.Lock()
	defer a.mu.Unlock()
	last := a.lastLEDTheme
	if a.led == nil || last == 0 || last == led.ThemeIgnore {
		return
	}
	if err := a.led.Off(); err != nil {
		log.Printf("app: led off on shutdown: %v", err)
		return
	}
	log.Printf("app: led off sent on shutdown")
}

// ---- editor / admin hooks ----

// DeviceRect maps a logical theme rect to the physical 320x170 buffer space
// (the same math as translate_rect in the original main.js: portrait user
// space (u,v) maps to buffer (v, 170-(u+w)) after the pre-rotation).
func DeviceRect(r config.Rect, portrait bool) config.Rect {
	if !portrait {
		return r
	}
	return config.Rect{
		X:      r.Y,
		Y:      display.FrameHeight - (r.X + r.Width),
		Width:  r.Height,
		Height: r.Width,
	}
}

// clampToFrame intersects a device-space rect with the panel frame, so the
// wire rect display.Update receives always matches the Frame.Region blob
// byte count even for rects that stick out of the screen.
func clampToFrame(r config.Rect, fw, fh int) config.Rect {
	if r.X < 0 {
		r.Width += r.X
		r.X = 0
	}
	if r.Y < 0 {
		r.Height += r.Y
		r.Y = 0
	}
	if x1 := r.X + r.Width; x1 > fw {
		r.Width -= x1 - fw
	}
	if y1 := r.Y + r.Height; y1 > fh {
		r.Height -= y1 - fh
	}
	if r.Width < 0 {
		r.Width = 0
	}
	if r.Height < 0 {
		r.Height = 0
	}
	return r
}

// Theme returns the live theme (do not mutate).
func (a *App) Theme() *config.Theme {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.theme
}

// SwapTheme hot-swaps the live theme; the next poll re-applies orientation,
// resets screen rotation and forces a full redraw.
func (a *App) SwapTheme(t *config.Theme) {
	a.mu.Lock()
	a.theme = t
	a.themeDirty = true
	// The old screenIdx can be out of range for the new theme (shorter
	// screens list): every indexer must never see the stale value.
	a.screenIdx = 0
	a.screenSince = time.Now()
	a.mu.Unlock()
	a.warnUnknownSensors(t)
}

// warnUnknownSensors logs every widget whose sensor the registry does not
// sample. Such widgets render "N/A"/blank (widgets.go looks up rd[w.Sensor]
// and finds no key); without this line the only trace is the registry's
// rate-limited sampling error, which never mentions the theme.
func (a *App) warnUnknownSensors(t *config.Theme) {
	known := make(map[string]bool)
	for _, n := range a.registry.Names() {
		known[n] = true
	}
	for _, sc := range t.Screens {
		for _, w := range sc.Widgets {
			if w.Sensor != "" && !known[w.Sensor] {
				log.Printf("app: theme: screen %q (id %d) widget %d (%s) references unknown sensor %q — it will render N/A/blank",
					sc.Name, sc.ID, w.ID, w.Type, w.Sensor)
			}
		}
	}
}

// Config returns the daemon configuration. Everything but theme_file is
// fixed at load time and safe to read through this pointer; the live
// theme_file is guarded by ThemeFile/SetThemeFile (the editor's
// /api/theme used to write cfg.ThemeFile unlocked while the poll loop
// read it).
func (a *App) Config() *config.Config { return a.cfg }

// ThemeFile is the live theme file path (locked).
func (a *App) ThemeFile() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.ThemeFile
}

// SetThemeFile updates the live theme file path (locked, in memory only —
// config.json on disk is never rewritten).
func (a *App) SetThemeFile(path string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cfg.ThemeFile = path
}

// ManualLED reports the LED settings last pushed from the editor, if any
// (they act as the live global until restart, outranking config.json).
func (a *App) ManualLED() (config.LEDSettings, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.manualLED == nil {
		return config.LEDSettings{}, false
	}
	return *a.manualLED, true
}

// CurrentScreenIndex is the screen currently shown (editor highlight).
func (a *App) CurrentScreenIndex() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.screenIdx
}

// LatestReadings returns the most recent sampled telemetry.
func (a *App) LatestReadings() map[string]metrics.Reading {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]metrics.Reading, len(a.readings))
	maps.Copy(out, a.readings)
	return out
}

// SensorNames lists configured sensors (editor dropdown).
func (a *App) SensorNames() []string { return a.registry.Names() }

// PreviewFrame renders a draft theme to a frame without touching hardware.
func (a *App) PreviewFrame(t *config.Theme, screenIdx int) (*render.Frame, error) {
	if screenIdx < 0 || screenIdx >= len(t.Screens) {
		return nil, fmt.Errorf("app: screen %d out of range", screenIdx)
	}
	_, _, portrait := t.OrientationSize()
	return render.Render(&t.Screens[screenIdx], t, display.FrameWidth, display.FrameHeight, portrait, a.LatestReadings(), a.fonts)
}

// PreviewOnScreen renders the draft and pushes it to the REAL panel via a
// full redraw (the editor "Примерить" button), then holds the live loop
// quiet for previewHold so the draft stays visible.
func (a *App) PreviewOnScreen(ctx context.Context, t *config.Theme, screenIdx int) (*render.Frame, error) {
	frame, err := a.PreviewFrame(t, screenIdx)
	if err != nil {
		return nil, err
	}
	if err := a.disp.Redraw(ctx, frame.Pixels); err != nil {
		return nil, err
	}
	a.setLastFrame(frame)
	a.mu.Lock()
	a.previewUntil = time.Now().Add(previewHold)
	a.forceRedraw = true
	a.previewLED = a.led != nil // tick restores the live screen's LED on expiry
	a.mu.Unlock()
	a.applyScreenLED(&t.Screens[screenIdx])
	return frame, nil
}
