// Package metrics samples system telemetry for the panel widgets
// (doc/revrite.md §1: gopsutil + /sys/class/hwmon for temperatures).
//
// Sensor names and format-token indices match the original Node sensors
// (s1panel/sensors/*.js) one-for-one so original theme files keep working:
// clock, calendar, cpu_usage, cpu_temp, cpu_power, memory, space_<mount>,
// network_<iface>.
package metrics

import (
	"log"
	"sync"
	"time"
)

// Series is a numeric history bound to a format-token index. The original
// widgets received chart data as a comma-joined string in the format token
// (value.split(',')), so the token that carries the joined history also
// selects which series a chart plots and which scale applies to it.
type Series struct {
	Values []float64 // oldest .. newest
	Max    float64   // per-token upper scale (original _max side-effect); 0 = autoscale
}

// Reading is one sampled metric.
type Reading struct {
	Value   float64        // primary numeric value
	Min     float64        // natural lower bound (usually 0)
	Max     float64        // natural upper bound; 0 = unbounded/unknown
	Text    []string       // pre-formatted strings for {0}..{n} format tokens (Text[0] is the default display)
	History []float64      // primary series (oldest .. newest), used when Format selects no token
	Series  map[int]Series // format-token index -> chart series (join-token of that sensor)
	When    time.Time
}

// SeriesAt resolves the chart series selected by a format-token index:
// the bound Series if present, otherwise the primary History.
func (r Reading) SeriesAt(token int) Series {
	if s, ok := r.Series[token]; ok {
		return s
	}
	return Series{Values: r.History, Max: r.Max}
}

// Sensor is a named metric source.
type Sensor interface {
	Name() string
	Sample() (Reading, error)
}

// Registry keeps an ordered set of sensors and samples them on demand.
type Registry struct {
	mu      sync.Mutex
	names   []string
	sensors map[string]Sensor
	lastErr map[string]time.Time
}

// NewRegistry builds a registry from the given sensors (duplicates by name
// are ignored, first registration wins).
func NewRegistry(sensors ...Sensor) *Registry {
	r := &Registry{sensors: map[string]Sensor{}, lastErr: map[string]time.Time{}}
	for _, s := range sensors {
		r.Add(s)
	}
	return r
}

// Add registers one sensor.
func (r *Registry) Add(s Sensor) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.sensors[s.Name()]; !dup {
		r.names = append(r.names, s.Name())
		r.sensors[s.Name()] = s
	}
}

// Names lists configured sensor names, in registration order (drives the
// editor's sensor dropdown).
func (r *Registry) Names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.names))
	copy(out, r.names)
	return out
}

// SampleAll samples every sensor. Failing sensors are skipped with a
// rate-limited log so missing hardware never takes the display down; the
// corresponding widgets render "N/A". Every sensor is sampled every tick,
// which also covers the original poll_inactive_screen_sensors behaviour.
func (r *Registry) SampleAll() map[string]Reading {
	out := make(map[string]Reading, len(r.names))
	for _, name := range r.Names() {
		reading, err := r.sensors[name].Sample()
		if err != nil {
			r.logOnce(name, err)
			continue
		}
		out[name] = reading
	}
	return out
}

func (r *Registry) logOnce(name string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.lastErr[name]; ok && time.Since(t) < time.Minute {
		return
	}
	r.lastErr[name] = time.Now()
	log.Printf("metrics: %s: %v", name, err)
}
