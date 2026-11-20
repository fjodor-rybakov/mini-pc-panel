package metrics

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---- cpu_temp (s1panel/sensors/cpu_temp.js) ----

// TempOptions configures the cpu_temp sensor. Chips/Labels widen the
// original hard-coded 'coretemp' + 'Package id 0' match (the Go port is a
// superset: any hwmon chip). Max/Min override the scale; when 0 the
// original rule applies: max = sibling tempN_max (once), else 105 °C /
// 230 °F; min = 21 °C / 70 °F.
type TempOptions struct {
	Chips      []string
	Labels     []string
	Fahrenheit bool
	MaxPoints  int
	Max        float64 // explicit upper bound in display units (°C or °F)
	Min        float64 // explicit lower bound in display units
}

// HwmonTemp returns a Sensor named "cpu_temp" (gopsutil v4 no longer ships
// host sensors; the spec §1 mandates the sysfs route). root is normally
// "/sys/class/hwmon" and is overridable for tests.
// Format tokens: {0} last temperature, {1} history joined, {2} "F"|"C".
func HwmonTemp(root string, opts TempOptions) Sensor {
	return &tempSensor{root: root, opts: opts, hist: prefilledRing(opts.MaxPoints)}
}

type tempSensor struct {
	root string
	opts TempOptions
	hist *ring
	init bool
	maxT float64
	minT float64
}

func (tempSensor) Name() string { return "cpu_temp" }

func (s *tempSensor) Sample() (Reading, error) {
	hit, err := findHwmon(s.root, s.opts.Chips, s.opts.Labels, "temp")
	if err != nil {
		return Reading{}, fmt.Errorf("cpu_temp: %w", err)
	}
	value := hit.raw / 1000 // millidegrees -> °C
	// The original converts the sibling tempN_max as well (cpu_temp.js
	// line 117-126) and adopts it as _max_temp on the first sample.
	sibMax := 0.0
	if hit.label != "" {
		if raw, err := strconv.ParseFloat(readTrim(strings.Replace(hit.label, "_label", "_max", 1)), 64); err == nil {
			sibMax = raw / 1000
		}
	}
	if s.opts.Fahrenheit {
		value = celsiusToFahrenheit(value)
		if sibMax > 0 {
			// Only convert a REAL sibling max: an absent one (0) would
			// become 32.0 °F, then satisfy the "sibMax > 0" case below and
			// set Max=32 < Min=70 - the 230 °F default was unreachable.
			sibMax = celsiusToFahrenheit(sibMax)
		}
	}
	if !s.init {
		switch {
		case s.opts.Max > 0:
			s.maxT = s.opts.Max
		case sibMax > 0:
			s.maxT = sibMax
		case s.opts.Fahrenheit:
			s.maxT = 230
		default:
			s.maxT = 105
		}
		switch {
		case s.opts.Min > 0:
			s.minT = s.opts.Min
		case s.opts.Fahrenheit:
			s.minT = 70
		default:
			s.minT = 21
		}
		s.init = true
	}
	s.hist.push(value)
	vals := s.hist.values()
	unit := "C"
	if s.opts.Fahrenheit {
		unit = "F"
	}
	return Reading{
		Value: value, Min: s.minT, Max: s.maxT,
		Text:    []string{fmt.Sprintf("%.0f", value), joinSeries(vals, "%.0f"), unit},
		History: vals,
		Series:  map[int]Series{1: {Values: vals, Max: s.maxT}},
		When:    time.Now(),
	}, nil
}

func celsiusToFahrenheit(c float64) float64 { return c*9/5 + 32 }

// ---- cpu_power (s1panel/sensors/cpu_power.js) ----

// CPUPower returns a Sensor named "cpu_power": CPU package power draw in
// watts, primary source /sys/class/powercap/*/energy_uj (RAPL). Unlike the
// original it pairs zones by path (a zone disappearing no longer shifts a
// positional pairing), reads only top-level zones (intel-rapl:N) so
// sub-zones (intel-rapl:N:M) are not double counted, clamps energy_uj
// wrap-arounds via max_energy_range_uj, and always divides the energy
// delta by the real sample gap (behaviour change, see README "Отличия").
// When no RAPL zone is readable the Go port additionally falls back to
// hwmon power*_input (µW) — an extension; the original reported 0 W then.
// Format tokens: {0} last watts, {1} history joined (max 28 like the
// original default doughnut scale).
func CPUPower(raplRoot, hwmonRoot string, chips, labels []string, maxPoints int) Sensor {
	return &powerSensor{
		raplRoot: raplRoot, hwmonRoot: hwmonRoot,
		chips: chips, labels: labels, hist: prefilledRing(maxPoints),
	}
}

type powerSensor struct {
	raplRoot, hwmonRoot string
	chips, labels       []string
	hist                *ring
	prev                map[string]float64 // last energy_uj per zone file
	prevTime            time.Time
}

func (powerSensor) Name() string { return "cpu_power" }

func (s *powerSensor) Sample() (Reading, error) {
	now := time.Now()
	zones, _ := filepath.Glob(filepath.Join(s.raplRoot, "*", "energy_uj"))
	zones = onlyTopLevelZones(zones)
	watts := 0.0
	if len(zones) > 0 {
		cur := make(map[string]float64, len(zones))
		for _, z := range zones {
			if n, err := strconv.ParseFloat(readTrim(z), 64); err == nil {
				cur[z] = n
			}
		}
		// Pair zones by path: the original paired them positionally, so a
		// zone disappearing mid-run silently subtracted one zone's energy
		// from another's previous value.
		for path, v := range cur {
			prev, ok := s.prev[path]
			if !ok {
				continue
			}
			delta := v - prev
			if delta < 0 {
				// energy_uj is a free-running counter that wraps at
				// max_energy_range_uj; the original counted the wrap as a
				// huge negative spike. Clamp it back into range; an
				// unparseable range means we cannot trust the sample.
				maxPath := filepath.Join(filepath.Dir(path), "max_energy_range_uj")
				if mr, err := strconv.ParseFloat(readTrim(maxPath), 64); err == nil && mr > 0 && prev <= mr {
					delta = v + mr - prev
				} else {
					delta = 0
				}
			}
			watts += delta / 1_000_000
		}
		// Original quirk (cpu_power.js line 142-153): the per-sample energy
		// was divided by the elapsed seconds only when that gap exceeded 1 s,
		// so sub-second ticks reported raw microjoules as watts. Always
		// divide by the real gap now (behaviour change, see README).
		if !s.prevTime.IsZero() {
			if dt := now.Sub(s.prevTime).Seconds(); dt > 0 {
				watts /= dt
			}
		}
		s.prev, s.prevTime = cur, now
	} else {
		hit, err := findHwmon(s.hwmonRoot, s.chips, s.labels, "power")
		if err != nil {
			return Reading{}, fmt.Errorf("cpu_power: no RAPL zones under %s and no hwmon fallback: %w", s.raplRoot, err)
		}
		watts = hit.raw / 1e6 // microwatts -> W
	}
	s.hist.push(watts)
	vals := s.hist.values()
	return Reading{
		Value: watts, Min: 0, Max: 28,
		Text:    []string{fmt.Sprintf("%.0f", watts), joinSeries(vals, "%.0f")},
		History: vals,
		Series:  map[int]Series{1: {Values: vals, Max: 28}},
		When:    now,
	}, nil
}

// onlyTopLevelZones keeps RAPL top-level zones (intel-rapl:N) and drops
// sub-zones (intel-rapl:N:M): the kernel reports sub-zone energy as part
// of its parent, so summing both double counts (package + core/dimm).
// If nothing looks top-level (unusual naming), keep everything rather
// than report 0 W.
func onlyTopLevelZones(zones []string) []string {
	var top []string
	for _, z := range zones {
		if strings.Count(filepath.Base(filepath.Dir(z)), ":") <= 1 {
			top = append(top, z)
		}
	}
	if len(top) == 0 {
		return zones
	}
	return top
}

// ---- shared hwmon discovery ----

// hwmonHit is one matching *_input file plus the label file it matched
// (empty for the bare <base>1_input fallback), and the raw sysfs value.
type hwmonHit struct {
	input string
	label string
	raw   float64
}

// findHwmon picks the *_input file whose sibling *_label matches the
// preferred label list (earlier label wins) on a chip whose "name" file
// matches the chip filter. Falls back to the first <base>1_input on a
// wanted chip when no labels match. base is "temp" or "power".
func findHwmon(root string, chips, labels []string, base string) (hwmonHit, error) {
	dirs, err := filepath.Glob(filepath.Join(root, "hwmon*"))
	if err != nil {
		return hwmonHit{}, err
	}
	if len(dirs) == 0 {
		return hwmonHit{}, fmt.Errorf("no hwmon dirs under %s", root)
	}
	sort.Strings(dirs)

	wanted := make(map[string]int, len(labels))
	for i, l := range labels {
		wanted[strings.ToLower(l)] = i
	}

	bestRank := -1
	var best hwmonHit
	for _, dir := range dirs {
		if !chipWanted(chips, readTrim(filepath.Join(dir, "name"))) {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), base) || !strings.HasSuffix(e.Name(), "_label") {
				continue
			}
			label := readTrim(filepath.Join(dir, e.Name()))
			if rank, ok := wanted[strings.ToLower(label)]; ok {
				if bestRank < 0 || rank < bestRank {
					bestRank = rank
					best = hwmonHit{
						input: filepath.Join(dir, strings.TrimSuffix(e.Name(), "_label")+"_input"),
						label: filepath.Join(dir, e.Name()),
					}
				}
			}
		}
	}
	if best.input == "" {
		// Plain <base>1_input fallback. Walk the chip list in priority
		// order, not hwmonN directory order: with the default chips
		// (coretemp...nvme), a lexicographic dir scan could hand nvme's
		// power1_input to cpu_power. Without an explicit chip list, keep
		// the dir-order scan.
		for _, chip := range chips {
			for _, dir := range dirs {
				if !strings.EqualFold(readTrim(filepath.Join(dir, "name")), chip) {
					continue
				}
				cand := filepath.Join(dir, base+"1_input")
				if _, err := os.Stat(cand); err == nil {
					best.input = cand
					break
				}
			}
			if best.input != "" {
				break
			}
		}
		if best.input == "" && len(chips) == 0 {
			for _, dir := range dirs {
				cand := filepath.Join(dir, base+"1_input")
				if _, err := os.Stat(cand); err == nil {
					best.input = cand
					break
				}
			}
		}
	}
	if best.input == "" {
		return hwmonHit{}, fmt.Errorf("no %s input matching labels %v in chips %v", base, labels, chips)
	}

	raw := readTrim(best.input)
	n, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return hwmonHit{}, fmt.Errorf("%s: parse %q: %w", best.input, raw, err)
	}
	best.raw = n
	return best, nil
}

func chipWanted(chips []string, name string) bool {
	if len(chips) == 0 {
		return true
	}
	n := strings.ToLower(name)
	for _, c := range chips {
		if strings.ToLower(c) == n {
			return true
		}
	}
	return false
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
