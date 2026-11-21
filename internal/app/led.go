package app

// All CH340 LED-strip traffic for the App, split out of app.go: the
// per-screen/global/manual settings merge, standby darkening, manual
// editor apply, shutdown frame and hot-replug recovery. Every port write
// runs under a.mu — the rotation path and the editor handlers drive the
// same serial device, and interleaved FA..checksum frames latch garbage.

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/fjodor-rybakov/mini-pc-panel/internal/config"
	"github.com/fjodor-rybakov/mini-pc-panel/internal/led"
)

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
			a.ledBroken = true // CH340 replug: maybeReopenLED retries OpenAuto
		} else {
			log.Printf("app: led: off frame % X -> %s", led.OffPacket(), a.led.Name)
			a.lastLEDTheme = s.Theme
		}
	default:
		if err := a.led.Set(s.Theme, s.Intensity, s.Speed); err != nil {
			log.Printf("app: led set: %v", err)
			a.ledBroken = true
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
		a.ledBroken = true
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
			a.ledBroken = true
			return err
		}
	default:
		if err := a.led.Set(settings.Theme, settings.Intensity, settings.Speed); err != nil {
			a.ledBroken = true
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

// maybeReopenLED retries the CH340 open after a failed port write (a
// replug) or when the port never came up at startup, at most once per 15 s
// and always on the Run/tick goroutine. OpenAuto touches the serial
// subsystem, so it runs outside a.mu. Returns true when the strip was
// reopened; the caller re-pushes the current state (standby dark / live
// screen mode) so the physical strip matches what the daemon believes.
func (a *App) maybeReopenLED() bool {
	a.mu.Lock()
	need := (a.cfg.LED.Enabled == nil || *a.cfg.LED.Enabled) && (a.led == nil || a.ledBroken)
	if !need || time.Since(a.ledRetryAt) < 15*time.Second {
		a.mu.Unlock()
		return false
	}
	a.ledRetryAt = time.Now()
	a.mu.Unlock()
	dev, note, err := led.OpenAuto(a.cfg.LED.Port)
	if note != "" {
		log.Printf("led: reopen: %s", note)
	}
	if err != nil {
		// The OpenAuto diagnosis is long (it lists every visible port):
		// the retry stays on its 15 s cadence, the log line at most once
		// a minute.
		if time.Since(a.ledFailLogged) >= time.Minute {
			a.ledFailLogged = time.Now()
			log.Printf("led: reopen failed: %v (next attempt in 15 s)", err)
		}
		return false
	}
	a.mu.Lock()
	a.led = dev
	a.ledBroken = false
	a.lastLEDTheme = 0 // fresh controller: the daemon holds nothing out there
	a.mu.Unlock()
	log.Printf("led: reopened %s (VID=%s PID=%s)", dev.Name, dev.VID, dev.PID)
	return true
}

// LEDConfig returns a locked copy of the LED config block (port, enabled,
// global settings) so the editor can render the live values without
// racing the PersistLED swap under a.mu (the server's effectiveLED used
// to read cfg.LED unlocked).
func (a *App) LEDConfig() config.LEDConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.LED
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
