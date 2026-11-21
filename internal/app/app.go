// Package app wires config, HID panel, LED, metrics and rendering into the
// daemon lifecycle described in doc/revrite.md §5.
package app

import (
	"context"
	"fmt"
	"log"
	"maps"
	"strings"
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
	// lastBackoffLog rate-limits the "panel link behind" log; only the
	// Run/tick goroutine touches it, so it needs no lock.
	lastBackoffLog time.Time
	manualLED      *config.LEDSettings // editor-driven LED set (outranks config)
	lastLEDTheme   int                 // mode the strip now holds (0 = nothing sent)
	// Standby (software screen-off): the protocol has no power command, so
	// the screen is a black full redraw, the LED strip is off and the tick
	// stops rendering until the schedule window ends or the button flips.
	standbyOn        bool // effective state: button toggle or schedule window
	standbyApplied   bool // black frame + LED off already pushed to the panel
	standbySchedPrev bool // schedule membership at the previous check (edge detect)
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
		// OpenAuto resolves the CH340 by USB identity (VID 1a86) when the
		// configured port is missing or belongs to another adapter: a
		// wrong ttyUSB index is the classic "off went nowhere" failure,
		// and both the chosen port and the full port list land in the log.
		dev, note, err := led.OpenAuto(cfg.LED.Port)
		if note != "" {
			log.Printf("led: %s", note)
		}
		if err != nil {
			log.Printf("led: %v (continuing without LED control)", err)
		} else {
			log.Printf("led: opened %s (VID=%s PID=%s)", dev.Name, dev.VID, dev.PID)
			log.Printf("led: serial ports visible here: %s", portsSummary())
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
	// /powercap is docker-compose's bind-mount of
	// /sys/devices/virtual/powercap: Docker masks that canonical path and
	// some versions re-apply the mask over a bind-mount at the SAME path,
	// so the canonical root can be empty while the host has RAPL. The
	// nested root covers the vendor dir under powercap (intel-rapl/...).
	s = append(s, metrics.CPUPower([]string{"/sys/class/powercap", "/powercap", "/powercap/intel-rapl"}, "/sys/class/hwmon",
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

	// Build marker for deployment checks ("screen did not change" vs
	// "an old image is still running"): docker compose logs | grep "pacing v4".
	// v4 = standby outranks the editor preview («Примерить» no longer
	// repaints the black standby frame).
	log.Printf("app: display pacing v4 (paced redraw chunks; standby wins over editor preview)")

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

	// first full frame (startup sequence: orientation -> time -> redraw).
	// Inside the standby window the very first frame is black: booting at
	// 23:30 with a 23:00-07:00 schedule must not flash the live screen for
	// one second before the tick's standby entry blacks it out.
	a.refresh()
	if a.cfg.Standby.Active(time.Now()) {
		a.mu.Lock()
		a.standbyOn, a.standbySchedPrev, a.standbyApplied = true, true, true
		a.mu.Unlock()
		if err := a.disp.Redraw(ctx, make([]byte, display.FrameBytes)); err != nil {
			return fmt.Errorf("app: initial standby redraw: %w", err)
		}
		a.turnStandbyLEDOff()
		log.Printf("app: standby window active at startup: screen kept black until %s", a.cfg.Standby.End)
		a.markHeartbeatSent()
		return a.runLoop(ctx)
	}
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
	return a.runLoop(ctx)
}

// runLoop carries the heartbeat/poll tickers of Run, so the standby-at-startup
// branch and the normal startup branch share one exit path.
func (a *App) runLoop(ctx context.Context) error {
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
		log.Printf("app: display write failures %d -> %d: panel is out of sync, full redraw queued", a.lastWriteFails, fails)
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
	// Standby state: the schedule flips it at the window boundaries (that
	// edge overrides any manual button toggle), the button flips it in
	// between. standbyApplied remembers the black frame is on the panel, so
	// entry happens once and exit latches exactly one full repaint.
	sched := a.cfg.Standby.Active(time.Now())
	standby, applied := a.standbyOn, a.standbyApplied
	if sched != a.standbySchedPrev {
		a.standbySchedPrev = sched
		a.standbyOn = sched
		standby, applied = sched, false
		log.Printf("app: standby schedule boundary: screen %s", map[bool]string{true: "off", false: "on"}[sched])
	}
	if standby {
		a.standbyApplied = true
	}
	a.mu.Unlock()

	// Standby (software screen-off). doc/revrite.md §2 has no power command,
	// so "off" is one black full frame, the strip dark, and no rotation or
	// rendering at all; heartbeats keep flowing (separate ticker) and the
	// editor metrics keep sampling. An active editor preview does NOT delay
	// standby: a "previewing" draft must never outrank an explicit
	// screen-off (field report "Экран не выключается" - the black frame was
	// skipped while the 5 s preview held, then the preview repainted).
	if standby {
		if !applied {
			if n := a.disp.DropPending(); n > 0 {
				log.Printf("app: dropping %d queued update job(s) before standby black frame", n)
			}
			if err := a.disp.Redraw(ctx, make([]byte, display.FrameBytes)); err != nil {
				return err
			}
			a.turnStandbyLEDOff()
			log.Printf("app: standby on: black frame sent, LED strip off")
		}
		a.refresh()
		return nil
	}
	if !standby && applied {
		a.mu.Lock()
		a.standbyApplied = false
		a.forceRedraw = true // panel holds a black frame lastFrame knows nothing about
		a.mu.Unlock()
		a.applyScreenLED(a.currentScreen())
		log.Printf("app: standby off: full repaint and LED restore queued")
	}

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

	a.mu.Lock()
	last := a.lastFrame
	full := a.forceRedraw
	themeRefresh := a.theme.Refresh == "redraw"
	a.mu.Unlock()

	// Upstream main.js only sends the next update after the lcd worker's
	// "complete" upcall, so its queue never backs up; our post() is
	// fire-and-forget. On a slow link the tiles of older frames would
	// otherwise keep painting onto the panel band by band - the visible
	// "shifted/torn strips" symptom. While jobs are still queued, defer
	// this tick's partial refresh: lastFrame stays the last SENT frame, so
	// the newest frame is diffed against it next tick and no stale tiles
	// enter the queue. Full redraws still go through (DropPending below).
	if pending := a.disp.Pending(); !full && !themeRefresh && last != nil && pending > 0 {
		a.noteBacklog(pending)
		return nil
	}

	frame, err := a.renderCurrent()
	if err != nil {
		return err
	}

	if last == nil || full || themeRefresh {
		// A full redraw repaints everything the queued partials were about
		// to paint; letting them run first only shows stale bands under it.
		if n := a.disp.DropPending(); n > 0 {
			log.Printf("app: dropping %d stale queued update job(s) before full redraw", n)
		}
		if err := a.disp.Redraw(ctx, frame.Pixels); err != nil {
			return err
		}
		a.setLastFrame(frame)
		a.mu.Lock()
		a.forceRedraw = false
		a.mu.Unlock()
		return nil
	}

	// Partial refresh: diff at TILE level, not per widget. A clock whose
	// seconds digit ticks usually changes one or two SplitRect tiles; the
	// widget-level check re-sent the whole clock region every second.
	// Every skipped tile is a write + 10 ms pause the link does not spend,
	// and the "one half updates, the other lags ~0.5 s" symptom is exactly
	// the panel painting too many tiles in sequence. All changed tiles of
	// the whole tick go as ONE Updates job, so they stay contiguous on the
	// wire (no heartbeat or next-tick pieces interleaving mid-frame).
	sc := a.currentScreen()
	_, _, portrait := a.Theme().OrientationSize()
	var tiles []display.Rect
	for _, w := range sc.Widgets {
		dr := clampToFrame(DeviceRect(w.Rect, portrait), frame.Width, frame.Height)
		if dr.Width <= 0 || dr.Height <= 0 {
			continue
		}
		if last.SameRegion(frame, dr.X, dr.Y, dr.Width, dr.Height) {
			continue
		}
		for _, sub := range display.SplitRect(display.Rect{X: dr.X, Y: dr.Y, Width: dr.Width, Height: dr.Height}) {
			if last.SameRegion(frame, sub.X, sub.Y, sub.Width, sub.Height) {
				continue
			}
			tiles = append(tiles, sub)
		}
	}
	if len(tiles) == 0 {
		a.setLastFrame(frame)
		return nil
	}
	// A full redraw is ChunkCount packets - fewer than tiling and one
	// coherent image once most of the screen moved anyway. Streaming that
	// many tiles band by band is what reads as lagging strips, so switch
	// to the redraw instead (its chunks are paced like every other packet:
	// this firmware drops/ignores unpaced redraw chunks).
	if len(tiles) >= display.ChunkCount {
		if n := a.disp.DropPending(); n > 0 {
			log.Printf("app: dropping %d stale queued update job(s) before full redraw", n)
		}
		if err := a.disp.Redraw(ctx, frame.Pixels); err != nil {
			return err
		}
		a.setLastFrame(frame)
		return nil
	}
	if err := a.disp.Updates(ctx, tiles, func(r display.Rect) []byte {
		return frame.Region(r.X, r.Y, r.Width, r.Height)
	}); err != nil {
		// Earlier jobs may already be on the panel: the screen is now a
		// mix of old and new frames. Don't adopt this frame as the
		// baseline - resync with a full redraw next tick.
		a.mu.Lock()
		a.forceRedraw = true
		a.mu.Unlock()
		return err
	}
	a.setLastFrame(frame)
	return nil
}

// noteBacklog logs the deferral at most once a minute (a slow link makes it
// fire on many consecutive ticks). Only the Run/tick goroutine calls it.
func (a *App) noteBacklog(pending int) {
	if time.Since(a.lastBackoffLog) < time.Minute {
		return
	}
	a.lastBackoffLog = time.Now()
	log.Printf("app: panel link behind (%d queued job(s)): deferring partial refresh; raise packet_delay/redraw_delay in config.json if this persists", pending)
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

// Standby forces software standby (black screen + strip off) on/off from
// the editor. Like PauseRotation it only flips the flag: every panel write
// stays on the Run/tick goroutine, which reacts within cfg.Poll ms. The
// schedule boundary outranks the button when the window opens or closes.
func (a *App) Standby(on bool) {
	a.mu.Lock()
	if a.standbyOn != on {
		a.standbyOn = on
		log.Printf("app: standby %s requested from editor", map[bool]string{true: "on", false: "off"}[on])
	}
	a.mu.Unlock()
}

// StandbyState reports the effective standby state (/api/state feeds the
// editor checkbox its initial value).
func (a *App) StandbyState() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.standbyOn
}

// StandbySchedule returns the configured standby window ("no timer" = both
// empty), so the editor can refill its schedule controls on load.
func (a *App) StandbySchedule() config.StandbyConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.Standby
}

// SetStandbySchedule changes the standby timer at runtime: it validates,
// rewrites config.json (same persistence the manual LED apply uses via
// SaveLED) and swaps the window into the live config. The next poll tick
// sees the schedule edge and applies it: a window covering "now" blacks
// the screen right away; clearing both times removes the timer entirely,
// after which only the button switches the screen off and nothing - no
// boundary, no window - ever switches it back on.
func (a *App) SetStandbySchedule(sc config.StandbyConfig) error {
	if err := sc.Validate(); err != nil {
		return err
	}
	if err := config.SaveStandby(a.cfg, sc); err != nil {
		return err
	}
	a.mu.Lock()
	a.cfg.Standby = sc
	a.mu.Unlock()
	if sc.Enabled() {
		log.Printf("app: standby timer set: %s-%s", sc.Start, sc.End)
	} else {
		log.Printf("app: standby timer removed: screen off is manual only")
	}
	return nil
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
// settings field by field (zero = inherit). The original overwrote all
// three fields wholesale on screen change (main.js:296-298, theme||4) and
// only set update_led then - there is no periodic re-apply loop: a frame
// goes out at startup, on screen changes INTO screens that HAVE led_config,
// and on editor applies. Shipped themes carry no per-screen led_config at
// all, so in practice the global/manual side drives the strip; inheriting
// missing fields from the global settings is the documented deviation.
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
			log.Printf("app: led: off frame % X -> %s", led.OffPacket(), a.led.Name)
			a.lastLEDTheme = s.Theme
		}
	default:
		if err := a.led.Set(s.Theme, s.Intensity, s.Speed); err != nil {
			log.Printf("app: led set: %v", err)
		} else {
			log.Printf("app: led: frame % X -> %s", led.Packet(s.Theme, s.Intensity, s.Speed), a.led.Name)
			a.lastLEDTheme = s.Theme
		}
	}
}

// turnStandbyLEDOff darkens the strip for the standby window and records
// ThemeOff as the mode the CH340 now holds, so the standby exit
// (applyScreenLED) and shutdownLED see what is out there. Port writes go
// under a.mu like every other LED call.
func (a *App) turnStandbyLEDOff() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.led == nil {
		return
	}
	if err := a.led.Off(); err != nil {
		log.Printf("app: standby led off: %v", err)
		return
	}
	a.lastLEDTheme = led.ThemeOff
	log.Printf("app: standby led: off frame % X -> %s", led.OffPacket(), a.led.Name)
}

// ApplyLEDManual pushes explicit strip settings (editor "apply LED" action).
// Config-scale intensity/speed; theme 6 sends nothing (original ignore).
// The settings also become the live global for every later screen change,
// so an editor "off" is no longer undone by the next rotation. The HTTP
// handler persists them with PersistLED before calling this - the original
// set_led_strip rewrote config.json the same way (api.js:309-316) - so the
// choice survives a restart too.
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
		log.Printf("app: led: manual frame % X -> %s", manualFrame(settings), a.led.Name)
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
	log.Printf("app: led off sent on shutdown to %s", a.led.Name)
}

// portsSummary renders every serial port this process can see for the
// startup log ("ttyUSB0[067b:2303] ttyUSB1[1a86:7523] <- CH340"). One
// grep-able line that answers "is the CH340 even visible here?" - wrong
// ttyUSB indices and missing docker device mappings both fail exactly
// this question.
func portsSummary() string {
	ports, err := led.ListPorts()
	if err != nil {
		return "enumeration failed: " + err.Error()
	}
	if len(ports) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		part := fmt.Sprintf("%s[%s:%s]", p.Name, p.VID, p.PID)
		if led.IsCH340(p) {
			part += " <- CH340"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, " ")
}

// manualFrame is the exact bytes ApplyLEDManual just pushed; logging it
// makes a "still lit" report decidable from the daemon log alone.
func manualFrame(s config.LEDSettings) []byte {
	if s.Theme == led.ThemeOff {
		return led.OffPacket()
	}
	return led.Packet(s.Theme, s.Intensity, s.Speed)
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

// PersistLED writes the given strip settings into the config file on disk so
// the choice survives a daemon/container restart — the original GUI did the
// same on every set_led_strip (s1panel/api.js:309-316). Without it a manual
// "off" lived only in memory and the next start re-applied the config's
// theme and re-lit the strip, which is what made "выкл" look ignored.
func (a *App) PersistLED(s config.LEDSettings) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return config.SaveLED(a.cfg, s)
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
	// Field regression (Nov 2026, "Ничего не поменялось"): the editor's
	// «Примерить на экран» pushed a full redraw OVER the standby black frame
	// (log: a second "redraw: all 27 chunks on the wire" seconds after
	// "standby on"), while standbyApplied stayed true so nothing re-blacked
	// the panel. Standby wins: still render the frame so the browser gets
	// its PNG, but never touch the panel or the strip while the screen is
	// off.
	a.mu.Lock()
	standby := a.standbyOn
	a.mu.Unlock()
	if standby {
		log.Printf("app: preview not pushed: standby is on, the screen stays black (turn standby off to preview on hardware)")
		return frame, nil
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
