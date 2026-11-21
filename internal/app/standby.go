package app

// Standby (software screen-off) state machine and its editor hooks, split
// out of app.go. Effective state = schedule window or button toggle (the
// schedule boundary wins when the window opens or closes); every panel
// write (black frame, strip off) happens on the Run/tick goroutine — the
// editor handlers only flip flags.

import (
	"log"

	"github.com/fjodor-rybakov/mini-pc-panel/internal/config"
)

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
// SaveLED) and swaps the window into the live config. SaveStandby is
// file-only, so the live swap must happen here, under a.mu, AFTER the file
// write succeeded — a failed write leaves config.json and memory matching
// each other (the old shared mutation desynced them on save errors). The
// next poll tick sees the schedule edge and applies it: a window covering
// "now" blacks the screen right away; clearing both times removes the
// timer entirely, after which only the button switches the screen off and
// nothing - no boundary, no window - ever switches it back on.
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
