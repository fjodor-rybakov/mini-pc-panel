// Package config loads the daemon configuration and the screen theme.
//
// Layout follows doc/revrite.md §5: JSON config referencing a theme file;
// theme files use the widget/screen schema of the original Node.js version
// (removed from this repo on 2026-11-19). Its spelling is still accepted:
// "theme"/"refresh"/"theme_list"/"listen" config keys, widget
// "name"+boolean "sensor"+"font" CSS string, and the "duartion" typo.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// HexUint16 decodes a uint16 from a JSON number (31641) or a string
// ("0x04D9" / "1241"), so VID/PID can be written the way datasheets show them.
type HexUint16 uint16

// UnmarshalJSON implements json.Unmarshaler for "0x..." / decimal forms.
func (h *HexUint16) UnmarshalJSON(data []byte) error {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	switch t := v.(type) {
	case float64:
		*h = HexUint16(uint16(t))
	case string:
		n, err := strconv.ParseUint(strings.TrimSpace(t), 0, 16)
		if err != nil {
			return fmt.Errorf("config: cannot parse %q as uint16: %w", t, err)
		}
		*h = HexUint16(n)
	default:
		return fmt.Errorf("config: unexpected JSON type for uint16: %T", v)
	}
	return nil
}

// DeviceConfig identifies the panel's HID interface.
type DeviceConfig struct {
	VID       HexUint16 `json:"vid"`
	PID       HexUint16 `json:"pid"`
	Interface int       `json:"interface"` // MI_01 -> 1 (spec §2)
}

// LEDSettings are config-file values in the user scale used by the original
// config.json and GUI (like led_thread.js): 1 = dim/slow .. 5 = bright/fast.
// The wire protocol is inverted; led.Packet() applies the original
// fix_value() = min(5, max(6-num, 1)) so 1 on the wire means max/fast.
type LEDSettings struct {
	Theme     int `json:"theme"`     // 1 rainbow, 2 breathing, 3 color cycle, 4 off, 5 automatic, 6 ignore
	Intensity int `json:"intensity"` // 1 dim .. 5 bright (fix_value() inverts onto the wire)
	Speed     int `json:"speed"`     // 1 slow .. 5 fast (fix_value() inverts onto the wire)
}

// LEDConfig describes the CH340 serial adapter of the LED strip.
type LEDConfig struct {
	Port    string `json:"port"`
	Enabled *bool  `json:"enabled"`
	LEDSettings
}

// FontFiles maps a theme font family to TTF files on disk.
type FontFiles struct {
	Regular string `json:"regular"`
	Bold    string `json:"bold"`
}

// SensorsConfig selects which system metrics are sampled and named.
// The original listed sensors as an array of {module, config} entries; the
// Go port derives them from the mounts/interfaces below and these common
// per-sensor options (max_points / fahrenheit / scaling match the original
// per-sensor config keys).
type SensorsConfig struct {
	NetworkInterfaces []string `json:"network_interfaces"`
	MountPoints       []string `json:"mount_points"`
	HwmonChips        []string `json:"hwmon_chips"`       // /sys/class/hwmon/*/name filter
	HwmonTempLabels   []string `json:"hwmon_temp_labels"` // temp*_label preference order
	HwmonPowerLabels  []string `json:"hwmon_power_labels"`
	MaxPoints         int      `json:"max_points"` // history length per sensor (original default 300)
	Fahrenheit        bool     `json:"fahrenheit"` // cpu_temp display unit
	Scaling           float64  `json:"scaling"`    // network chart autoscale factor (original default 1.5)
}

// ThemeRef is one entry of the config's theme_list (admin editor picker).
// File uses the original "config" key so s1panel config.json loads unchanged.
type ThemeRef struct {
	Name string `json:"name"`
	File string `json:"config"`
}

// Config is the top level daemon configuration.
type Config struct {
	Device      DeviceConfig         `json:"device"`
	LED         LEDConfig            `json:"led"`
	ThemeFile   string               `json:"theme_file"`
	ThemeList   []ThemeRef           `json:"theme_list"` // selectable themes (original theme_list)
	Listen      string               `json:"listen"`     // admin/REST bind address
	DebugUpdate bool                 `json:"debug_update"`
	Poll        int                  `json:"poll"`         // ms between metric samples
	Heartbeat   int                  `json:"heartbeat"`    // ms heartbeat threshold (original 60000)
	RedrawDelay int                  `json:"redraw_delay"` // ms wait after a redraw job (original "refresh", 1600)
	PacketDelay int                  `json:"packet_delay"` // ms between HID packets, spec §5.1 mandates 10
	Fonts       map[string]FontFiles `json:"fonts"`
	Sensors     SensorsConfig        `json:"sensors"`

	// Dir is the directory of the config file; ThemeFile and font paths are
	// resolved relative to it. Not part of the JSON.
	Dir string `json:"-"`
}

// UnmarshalJSON accepts the original s1panel config.json spellings:
// "theme" as an alias of theme_file, numeric "refresh" as the redraw delay,
// "led_config" as the LED block, a string "device" (the original USB path
// "1-8:1.1" has no VID/PID, so HID defaults apply), and ignores the
// original "sensors" array (Go derives sensors itself).
func (c *Config) UnmarshalJSON(data []byte) error {
	type alias Config
	aux := struct {
		*alias
		Theme      string          `json:"theme"`
		Refresh    int             `json:"refresh"`
		SensorsRaw json.RawMessage `json:"sensors"`
		DeviceRaw  json.RawMessage `json:"device"`
		LedRaw     *struct {
			LEDSettings
			Device string `json:"device"` // original: serial port of the CH340
		} `json:"led_config"`
	}{alias: (*alias)(c)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if c.ThemeFile == "" && aux.Theme != "" {
		c.ThemeFile = aux.Theme
	}
	if c.RedrawDelay <= 0 && aux.Refresh > 0 {
		c.RedrawDelay = aux.Refresh
	}
	if len(aux.SensorsRaw) > 0 && aux.SensorsRaw[0] == '[' {
		c.Sensors = SensorsConfig{} // original module list: not applicable, start clean
	}
	if len(aux.DeviceRaw) > 0 && aux.DeviceRaw[0] == '{' {
		if err := json.Unmarshal(aux.DeviceRaw, &c.Device); err != nil {
			return err
		}
	}
	if aux.LedRaw != nil {
		if c.LED.Port == "" {
			c.LED.Port = aux.LedRaw.Device
		}
		if c.LED.Theme == 0 {
			c.LED.Theme = aux.LedRaw.Theme
		}
		if c.LED.Intensity == 0 {
			c.LED.Intensity = aux.LedRaw.Intensity
		}
		if c.LED.Speed == 0 {
			c.LED.Speed = aux.LedRaw.Speed
		}
	}
	return nil
}

// Load reads and validates the config file at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	cfg := &Config{}
	if err := json.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	cfg.applyDefaults()
	cfg.Dir = filepath.Dir(path)
	if cfg.ThemeFile != "" && !filepath.IsAbs(cfg.ThemeFile) {
		cfg.ThemeFile = filepath.Join(cfg.Dir, cfg.ThemeFile)
	}
	if cfg.ThemeFile == "" {
		return nil, fmt.Errorf("config: theme_file is required")
	}
	for i := range cfg.ThemeList {
		if cfg.ThemeList[i].File != "" && !filepath.IsAbs(cfg.ThemeList[i].File) {
			cfg.ThemeList[i].File = filepath.Join(cfg.Dir, cfg.ThemeList[i].File)
		}
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Device.VID == 0 {
		c.Device.VID = 0x04D9
	}
	if c.Device.PID == 0 {
		c.Device.PID = 0xFD01
	}
	if c.Device.Interface == 0 {
		c.Device.Interface = 1
	}
	if c.LED.Port == "" {
		c.LED.Port = "/dev/ttyUSB0"
	}
	if c.LED.Enabled == nil {
		on := true
		c.LED.Enabled = &on
	}
	if c.LED.Theme == 0 {
		c.LED.Theme = 5 // automatic
	}
	if c.LED.Intensity == 0 {
		c.LED.Intensity = 3
	}
	if c.LED.Speed == 0 {
		c.LED.Speed = 3
	}
	if c.Listen == "" {
		c.Listen = "0.0.0.0:8686" // original default
	}
	if c.Poll <= 0 {
		c.Poll = 1000
	}
	if c.Heartbeat <= 0 {
		c.Heartbeat = 60000 // original config.json threshold (see internal/display)
	}
	if c.RedrawDelay <= 0 {
		c.RedrawDelay = 1600 // original config.json "refresh"
	}
	if c.PacketDelay <= 0 {
		c.PacketDelay = 10
	}
	if len(c.Sensors.MountPoints) == 0 {
		c.Sensors.MountPoints = []string{"/"}
	}
	if len(c.Sensors.HwmonChips) == 0 {
		c.Sensors.HwmonChips = []string{"coretemp", "k10temp", "zenpower", "cpu_thermal", "acpitz", "nvme"}
	}
	if len(c.Sensors.HwmonTempLabels) == 0 {
		c.Sensors.HwmonTempLabels = []string{"Package id 0", "Tctl", "Tdie", "Composite", "temp1_input"}
	}
	if len(c.Sensors.HwmonPowerLabels) == 0 {
		c.Sensors.HwmonPowerLabels = []string{"Package power", "CPU power", "power1_input"}
	}
	if c.Sensors.MaxPoints <= 0 {
		c.Sensors.MaxPoints = 300 // original max_points default
	}
	if c.Sensors.Scaling <= 0 {
		c.Sensors.Scaling = 1.5 // original network.js scale_factor default
	}
}

// LEDEnabled reports whether the LED strip should be driven.
func (c *Config) LEDEnabled() bool { return c.LED.Enabled != nil && *c.LED.Enabled }

// Theme describes the screens drawn on the panel.
type Theme struct {
	Orientation string   `json:"orientation"` // "landscape" | "portrait"
	Refresh     string   `json:"refresh"`     // "update" (partial rects) | "redraw" (full frame each poll)
	Background  string   `json:"background"`
	Screens     []Screen `json:"screens"`

	// Dir is the directory of the theme file on disk; wallpaper and image
	// widget paths resolve against it. Not part of the JSON: drafts from
	// ParseTheme have no file, so their resources keep the cwd semantics.
	Dir string `json:"-"`
}

// OrientationSize returns the logical widget coordinate space of the theme.
// The physical framebuffer is always 320x170; portrait themes use a 170x320
// logical space that is pre-rotated into the landscape buffer (like the
// original implementation) and physically rotated by the device itself.
func (t *Theme) OrientationSize() (w, h int, portrait bool) {
	if t.Orientation == "portrait" {
		return 170, 320, true
	}
	return 320, 170, false
}

// Screen is one page shown before rotating. Duration ms; 0 or absent means
// the screen never rotates away on its own (original behaviour: main.js
// line 271 only expires a screen when duration is truthy). The effective
// wait is max(Duration, 10000), applied by the app, not here.
type Screen struct {
	ID         int          `json:"id"`
	Name       string       `json:"name"`
	Enabled    *bool        `json:"enabled"`
	Duration   int          `json:"duration"` // ms, 0 = never
	Wallpaper  string       `json:"wallpaper"`
	Background string       `json:"background"`
	LED        *LEDSettings `json:"led_config"`
	Widgets    []Widget     `json:"widgets"`
}

// IsEnabled reports whether the screen takes part in rotation.
func (s *Screen) IsEnabled() bool { return s.Enabled == nil || *s.Enabled }

// UnmarshalJSON also accepts the "duartion" typo found in original theme
// files (s1panel/themes/simple_demo/portrait_simple.json lines 241/652/761).
func (s *Screen) UnmarshalJSON(data []byte) error {
	type alias Screen
	aux := struct {
		*alias
		Duartion int `json:"duartion"`
	}{alias: (*alias)(s)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if s.Duration == 0 && aux.Duartion != 0 {
		s.Duration = aux.Duartion
	}
	return nil
}

// Rect is a widget bounding box in logical theme coordinates (top-left origin).
type Rect struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

// FontSpec selects a family declared in Config.Fonts (falls back to an
// embedded font when the family is unknown or its file cannot be loaded).
// Original themes write fonts as CSS strings: "17px Arial", "bold 18px Arial".
type FontSpec struct {
	Family string  `json:"family"`
	Size   float64 `json:"size"` // treated as mm == px at DPMM(1.0)
	Bold   bool    `json:"bold"`
}

var cssFontRe = regexp.MustCompile(`(?i)^(?:(bold)\s+)?([0-9]+(?:\.[0-9]+)?)px\s+(.+)$`)

// UnmarshalJSON accepts both the Go object form and the original CSS form.
func (f *FontSpec) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		m := cssFontRe.FindStringSubmatch(strings.TrimSpace(s))
		if m == nil {
			return fmt.Errorf("config: cannot parse font %q (want \"[bold] NNpx family\")", s)
		}
		size, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			return fmt.Errorf("config: font size %q: %w", m[2], err)
		}
		f.Bold = strings.EqualFold(m[1], "bold")
		f.Size = size
		f.Family = strings.TrimSpace(m[3])
		return nil
	}
	type alias FontSpec
	return json.Unmarshal(data, (*alias)(f))
}

// Widget is a single drawing element on a screen.
type Widget struct {
	ID            int      `json:"id"`
	Group         int      `json:"group"` // original grouping/ordering hint (kept for round-trip)
	Type          string   `json:"type"`  // text | doughnut | doughnut_chart | line_chart | bar_chart | image | custom_bar | none | spacer
	Rect          Rect     `json:"rect"`
	Sensor        string   `json:"sensor"` // metric name, see internal/metrics
	Value         string   `json:"value"`  // sensor name (original sensor:true) or literal text
	Format        string   `json:"format"` // template: {0}..{n} -> Reading.Text, {v}/{v1} -> Value
	Refresh       int      `json:"refresh"`
	Color         string   `json:"color"` // text color (empty -> renderer default)
	Align         string   `json:"align"` // left | center | right
	Font          FontSpec `json:"font"`
	Fill          string   `json:"fill"`    // chart area/bar fill
	Outline       string   `json:"outline"` // chart stroke / bar frame
	Used          string   `json:"used"`    // doughnut: used arc
	Free          string   `json:"free"`    // doughnut: background ring
	Thickness     float64  `json:"thickness"`
	Points        int      `json:"points"` // history points for charts (<= 0 -> 30)
	Horizontal    bool     `json:"horizontal"`
	Area          bool     `json:"area"`          // line chart: fill under the curve
	Zoom          float64  `json:"zoom"`          // bar_chart original autoscale hint
	Cutout        float64  `json:"cutout"`        // doughnut inner radius percent (0 -> 80)
	Rotation      float64  `json:"rotation"`      // doughnut start angle, Chart.js degrees (0 -> 225)
	Circumference float64  `json:"circumference"` // doughnut sweep (0 -> 270)
	Max           float64  `json:"max"`           // fixed upper scale (0 -> sensor max / autoscale)
	DebugFrame    bool     `json:"debug_frame"`   // original red 1px rect around the widget
}

// supportedWidgetTypes are widget renderers implemented by internal/render.
var supportedWidgetTypes = map[string]bool{
	"text": true, "doughnut": true, "doughnut_chart": true, "line_chart": true,
	"bar_chart": true, "image": true, "custom_bar": true, "none": true, "spacer": true,
}

// UnmarshalJSON accepts the original widget spelling: "name" carries the
// widget type, "sensor" is a boolean flag whose partner text lives in
// "value" (sensor:true) or is literal text (sensor:false), and "font" may be
// a CSS string.
func (w *Widget) UnmarshalJSON(data []byte) error {
	type alias Widget
	aux := struct {
		*alias
		Name   string          `json:"name"`
		Sensor json.RawMessage `json:"sensor"`
	}{alias: (*alias)(w)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	// The original spells widget types either in "name" (most widgets) or
	// in "type"; accept both spellings. Unknown types (weather/iconify from
	// the removed original, typos) pass through so Validate rejects them
	// with a helpful error.
	switch {
	case w.Type != "":
	case aux.Name != "":
		w.Type = aux.Name
	}
	switch {
	case len(aux.Sensor) == 0 || string(aux.Sensor) == "null":
		// no sensor flag: Value stays literal
	case aux.Sensor[0] == '"':
		var s string // Go spelling: "sensor": "cpu_temp"
		if err := json.Unmarshal(aux.Sensor, &s); err != nil {
			return err
		}
		w.Sensor = s
	case string(aux.Sensor) == "true":
		// original spelling: {"sensor": true, "value": "cpu_usage"}
		w.Sensor = w.Value
		w.Value = ""
	case string(aux.Sensor) == "false":
	case aux.Sensor[0] == '{':
		// GUI editor form: {"sensor": {"name": "..."}}
		var o struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(aux.Sensor, &o); err != nil {
			return err
		}
		w.Sensor = o.Name
	default:
		return fmt.Errorf("config: unexpected sensor field %s", aux.Sensor)
	}
	return nil
}

// LoadTheme reads, validates and defaults a theme file.
func LoadTheme(path string) (*Theme, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("theme: %w", err)
	}
	t := &Theme{}
	if err := json.Unmarshal(raw, t); err != nil {
		return nil, fmt.Errorf("theme: parse %s: %w", path, err)
	}
	if err := Validate(t); err != nil {
		return nil, fmt.Errorf("theme %s: %w", path, err)
	}
	t.Dir = filepath.Dir(path)
	return t, nil
}

// ParseTheme decodes a draft theme from the admin editor (POST body) with
// the same defaults and validation as theme files.
func ParseTheme(raw []byte) (*Theme, error) {
	t := &Theme{}
	if err := json.Unmarshal(raw, t); err != nil {
		return nil, fmt.Errorf("theme: parse draft: %w", err)
	}
	if err := Validate(t); err != nil {
		return nil, fmt.Errorf("theme draft: %w", err)
	}
	return t, nil
}

// SaveTheme writes a theme back to disk (editor "apply + save" action).
func SaveTheme(path string, t *Theme) error {
	raw, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return fmt.Errorf("theme: encode: %w", err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return fmt.Errorf("theme: write %s: %w", path, err)
	}
	return nil
}

// Validate applies defaults and checks invariants. Used both for files and
// for drafts coming from the admin editor. Like the original main.js
// (lines 634 and 837) screens and their widgets are ordered by id.
func Validate(t *Theme) error {
	if t.Orientation == "" {
		t.Orientation = "landscape"
	}
	if t.Orientation != "landscape" && t.Orientation != "portrait" {
		return fmt.Errorf("orientation must be \"landscape\" or \"portrait\", got %q", t.Orientation)
	}
	if t.Refresh == "" {
		t.Refresh = "update"
	}
	if t.Refresh != "update" && t.Refresh != "redraw" {
		return fmt.Errorf("refresh must be \"update\" or \"redraw\", got %q", t.Refresh)
	}
	if t.Background == "" {
		t.Background = "#000000"
	}
	if len(t.Screens) == 0 {
		return fmt.Errorf("at least one screen is required")
	}
	sort.SliceStable(t.Screens, func(i, j int) bool { return t.Screens[i].ID < t.Screens[j].ID })
	anyEnabled := false
	ow, oh, _ := t.OrientationSize()
	for i := range t.Screens {
		sc := &t.Screens[i]
		if sc.IsEnabled() {
			anyEnabled = true
		}
		sort.SliceStable(sc.Widgets, func(a, b int) bool { return sc.Widgets[a].ID < sc.Widgets[b].ID })
		for j := range sc.Widgets {
			w := &sc.Widgets[j]
			if w.Type == "" {
				w.Type = "text"
			}
			if !supportedWidgetTypes[w.Type] {
				return fmt.Errorf("screen %d widget %d: unknown type %q", sc.ID, w.ID, w.Type)
			}
			if w.Rect.Width <= 0 || w.Rect.Height <= 0 {
				return fmt.Errorf("screen %d widget %d: rect must have positive width/height", sc.ID, w.ID)
			}
			if w.Rect.X < 0 || w.Rect.Y < 0 || w.Rect.X+w.Rect.Width > ow || w.Rect.Y+w.Rect.Height > oh {
				return fmt.Errorf("screen %d widget %d: rect %dx%d@%d,%d does not fit the %dx%d theme",
					sc.ID, w.ID, w.Rect.Width, w.Rect.Height, w.Rect.X, w.Rect.Y, ow, oh)
			}
			// Original text.js falls back per-draw: color || 'white',
			// font || '20px Arial' (text.js line 43-44). Keep empty values
			// empty here so theme round-trips stay faithful; render applies
			// the fallback.
			if w.Type == "text" && w.Font.Size <= 0 {
				w.Font.Size = 20
			}
			if w.Type == "doughnut" || w.Type == "doughnut_chart" {
				if w.Thickness <= 0 {
					w.Thickness = 8
				}
			}
		}
	}
	if !anyEnabled {
		return fmt.Errorf("all screens are disabled")
	}
	return nil
}
