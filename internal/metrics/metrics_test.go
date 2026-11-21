package metrics

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

type fakeSensor struct {
	name string
	val  float64
	fail bool
}

func (f fakeSensor) Name() string { return f.name }
func (f fakeSensor) Sample() (Reading, error) {
	if f.fail {
		return Reading{}, errors.New("sample failed")
	}
	return Reading{Value: f.val}, nil
}

func TestRegistryDedupeAndOrder(t *testing.T) {
	r := NewRegistry(fakeSensor{"a", 1, false}, fakeSensor{"a", 2, false})
	r.Add(fakeSensor{"b", 3, true})
	names := r.Names()
	if len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Fatalf("Names = %v, want [a b] (first wins, registration order)", names)
	}
	rd := r.SampleAll()
	if len(rd) != 1 {
		t.Fatalf("SampleAll kept %d entries, want 1 (failing sensor skipped)", len(rd))
	}
	if rd["a"].Value != 1 {
		t.Fatalf("first registration must win, got %v", rd["a"].Value)
	}
}

func TestRingCircularity(t *testing.T) {
	r := newRing(3)
	for _, v := range []float64{1, 2, 3, 4} {
		r.push(v)
	}
	got := r.values()
	if len(got) != 3 || got[0] != 2 || got[1] != 3 || got[2] != 4 {
		t.Fatalf("values = %v, want [2 3 4] oldest->newest", got)
	}
	r.push(5)
	got = r.values()
	if len(got) != 3 || got[0] != 3 || got[2] != 5 {
		t.Fatalf("values = %v, want [3 4 5]", got)
	}
}

func TestPrefilledRing(t *testing.T) {
	// Original histories are always exactly max_points long, zero-padded.
	r := prefilledRing(4)
	r.push(7)
	got := r.values()
	if len(got) != 4 || got[3] != 7 || got[0] != 0 {
		t.Fatalf("values = %v, want [0 0 0 7]", got)
	}
}

func TestSensorNamesMatchOriginal(t *testing.T) {
	want := map[string]string{
		"clock":          Clock().Name(),
		"calendar":       Calendar().Name(),
		"cpu_usage":      CPUUsage(1).Name(),
		"memory":         Memory(1).Name(),
		"space_root":     Space("/", 1).Name(),
		"space_home":     Space("/home", 1).Name(),
		"space_mnt_data": Space("/mnt/data", 1).Name(),
		"network_enp2s0": Network("enp2s0", 1, 1.5, "").Name(),
	}
	for wantName, got := range want {
		if wantName != got {
			t.Errorf("sensor name = %q, want %q", got, wantName)
		}
	}
}

func TestSeriesAtFallback(t *testing.T) {
	r := Reading{
		History: []float64{1, 2},
		Max:     5,
		Series:  map[int]Series{2: {Values: []float64{9}, Max: 100}},
	}
	if s := r.SeriesAt(0); len(s.Values) != 2 || s.Max != 5 {
		t.Fatalf("unbound token must fall back to primary history, got %+v", s)
	}
	if s := r.SeriesAt(2); len(s.Values) != 1 || s.Max != 100 {
		t.Fatalf("bound token must resolve its series, got %+v", s)
	}
}

func TestOrdinalSuffix(t *testing.T) {
	// Faithful to calendar.js: the 11..13 rule matches the raw number, not
	// the last two digits — 111 becomes "st" (unreachable for days 1..31).
	want := map[int]string{1: "st", 2: "nd", 3: "rd", 4: "th", 11: "th", 12: "th", 13: "th", 21: "st", 22: "nd", 23: "rd", 111: "st"}
	for n, sfx := range want {
		if got := ordinalSuffix(n); got != sfx {
			t.Errorf("ordinalSuffix(%d) = %q, want %q", n, got, sfx)
		}
	}
}

func TestFormatBytesLadder(t *testing.T) {
	want := map[float64]string{
		500:                    "500 B",
		1536:                   "1.50 KB",
		3 * 1024 * 1024:        "3.00 MB",
		2 * 1024 * 1024 * 1024: "2.00 GB",
	}
	for in, out := range want {
		if got := formatBytes(in); got != out {
			t.Errorf("formatBytes(%v) = %q, want %q", in, got, out)
		}
	}
}

func TestBytesToDataRate(t *testing.T) {
	// Byte mode: two decimals except raw bytes. Bit mode: Gbit/s 3 places,
	// Mbit/s 1, kbit/s 0, bit/s integer (network.js ladder).
	if got := bytesToDataRate(500, false); got != "500 B/s" {
		t.Errorf("got %q", got)
	}
	if got := bytesToDataRate(2048, false); got != "2.00 Kb/s" {
		t.Errorf("got %q", got)
	}
	if got := bytesToDataRate(2*1024*1024, false); got != "2.00 Mb/s" {
		t.Errorf("got %q", got)
	}
	// Ladder is /1024-based: 500 B = 4000 bit -> 3.9 kb -> "4 kbit/s".
	if got := bytesToDataRate(500, true); got != "4 kbit/s" {
		t.Errorf("got %q", got)
	}
	if got := bytesToDataRate(2048, true); got != "16 kbit/s" {
		t.Errorf("got %q", got)
	}
	if got := bytesToDataRate(1024*1024, true); got != "8.0 Mbit/s" {
		t.Errorf("got %q", got)
	}
	if got := bytesToDataRate(2*1024*1024*1024, true); got != "16.000 Gbit/s" {
		t.Errorf("got %q", got)
	}
}

func writeHWmon(t *testing.T, root, dir, chip string, files map[string]string) {
	t.Helper()
	d := filepath.Join(root, dir)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "name"), []byte(chip), 0o644); err != nil {
		t.Fatal(err)
	}
	for f, v := range files {
		if err := os.WriteFile(filepath.Join(d, f), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// Fake /sys/class/hwmon tree; values are sysfs-native (millidegrees /
// microwatts) and must be scaled by the sensor. temp2_max is the sibling
// cpu_temp.js adopts as the reading upper bound on its first sample.
func fakeHWmonTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeHWmon(t, root, "hwmon0", "coretemp", map[string]string{
		"temp1_label":  "Core 0",
		"temp1_input":  "35000",
		"temp2_label":  "PACKAGE id 0", // label matching is case-insensitive
		"temp2_input":  "45000",
		"temp2_max":    "90000",
		"power1_label": "Package power",
		"power1_input": "15000000",
	})
	writeHWmon(t, root, "hwmon1", "i915", map[string]string{
		"temp1_label": "Package id 0",
		"temp1_input": "99000", // only reachable when i915 is the wanted chip
	})
	return root
}

func TestHwmonTempPrefersLabel(t *testing.T) {
	s := HwmonTemp(fakeHWmonTree(t), TempOptions{Chips: []string{"coretemp"}, Labels: []string{"Package id 0", "Tctl"}, MaxPoints: 1})
	rd, err := s.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if rd.Value != 45 {
		t.Fatalf("Value = %v, want 45 °C (45000 millidegrees)", rd.Value)
	}
	// Reading Max comes from the sibling temp2_max (90000 -> 90), like the
	// original _max_temp; Min keeps the 21 °C default.
	if rd.Max != 90 || rd.Min != 21 {
		t.Fatalf("Min/Max = %v/%v, want 21/90", rd.Min, rd.Max)
	}
	if len(rd.Text) != 3 || rd.Text[0] != "45" || rd.Text[1] != "45" || rd.Text[2] != "C" {
		t.Fatalf("Text = %v, want [45 45 C]", rd.Text)
	}
	if len(rd.History) != 1 || rd.History[0] != 45 {
		t.Fatalf("History = %v, want [45]", rd.History)
	}
	if s := rd.SeriesAt(1); s.Max != 90 {
		t.Fatalf("join token series Max = %v, want 90", s.Max)
	}
}

func TestHwmonTempFahrenheit(t *testing.T) {
	s := HwmonTemp(fakeHWmonTree(t), TempOptions{Chips: []string{"coretemp"}, Labels: []string{"Package id 0"}, Fahrenheit: true, MaxPoints: 1})
	rd, err := s.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if rd.Value != 113 { // 45 °C -> 113 °F
		t.Fatalf("Value = %v, want 113 °F", rd.Value)
	}
	if rd.Max != 194 || rd.Min != 70 { // temp2_max 90 °C -> 194 °F; F floor 70
		t.Fatalf("Min/Max = %v/%v, want 70/194", rd.Min, rd.Max)
	}
	if rd.Text[2] != "F" {
		t.Fatalf("unit token = %q, want F", rd.Text[2])
	}
}

func TestHwmonTempFallbackToFirstInput(t *testing.T) {
	s := HwmonTemp(fakeHWmonTree(t), TempOptions{Chips: []string{"coretemp"}, Labels: []string{"NoSuchLabel"}, MaxPoints: 1})
	rd, err := s.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if rd.Value != 35 {
		t.Fatalf("Value = %v, want 35 (temp1_input fallback)", rd.Value)
	}
	if rd.Max != 105 {
		t.Fatalf("Max = %v, want 105 default when no sibling max applies", rd.Max)
	}
}

func TestHwmonTempChipFilter(t *testing.T) {
	s := HwmonTemp(fakeHWmonTree(t), TempOptions{Chips: []string{"i915"}, Labels: []string{"Package id 0"}, MaxPoints: 1})
	rd, err := s.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if rd.Value != 99 {
		t.Fatalf("Value = %v, want 99 (only the i915 chip may be read)", rd.Value)
	}
}

func TestHwmonTempNoMatchingChip(t *testing.T) {
	s := HwmonTemp(fakeHWmonTree(t), TempOptions{Chips: []string{"nonexistent"}, Labels: []string{"Package id 0"}, MaxPoints: 1})
	if _, err := s.Sample(); err == nil {
		t.Fatal("unknown chip must error, not silently return a value")
	}
}

// Fake /sys/class/powercap tree with one RAPL zone.
func fakePowercapTree(t *testing.T, energy string) string {
	t.Helper()
	root := t.TempDir()
	writeHWmon(t, root, "intel_cpu_00", "", map[string]string{"energy_uj": energy})
	return root
}

func TestCPUPowerRAPLDeltas(t *testing.T) {
	rapl := fakePowercapTree(t, "1000000")
	zone := filepath.Join(rapl, "intel_cpu_00")
	if err := os.WriteFile(filepath.Join(zone, "max_energy_range_uj"), []byte("4000000"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := CPUPower([]string{rapl}, fakeHWmonTree(t), []string{"coretemp"}, []string{"Package power"}, 4)
	rd, err := s.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if rd.Value != 0 {
		t.Fatalf("first sample = %v, want 0 (no previous counters)", rd.Value)
	}
	if err := os.WriteFile(filepath.Join(zone, "energy_uj"), []byte("3000000"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond) // dt ≥ 1.1 s keeps the watt bounds stable
	rd, err = s.Sample()
	if err != nil {
		t.Fatal(err)
	}
	// 2 000 000 µJ over ~1.1 s. Behaviour change: the energy delta is now
	// ALWAYS divided by the real gap (the original divided only when the
	// gap exceeded 1 s, reporting raw microjoules as watts on sub-second
	// ticks).
	if rd.Value < 1.3 || rd.Value > 2.2 {
		t.Fatalf("Value = %v W, want ~1.8 (2 J over ~1.1 s)", rd.Value)
	}
	if rd.Max != 28 {
		t.Fatalf("Max = %v, want 28 (original fixed scale)", rd.Max)
	}
	// energy_uj is a free-running counter that wraps at max_energy_range_uj.
	// The original counted the wrap as a -2.5 J negative spike; now it is
	// clamped: 500000 + 4000000 - 3000000 = 1.5 J over ~1.1 s.
	if err := os.WriteFile(filepath.Join(zone, "energy_uj"), []byte("500000"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	rd, err = s.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if rd.Value < 0.9 || rd.Value > 1.5 {
		t.Fatalf("Value = %v W after counter wrap, want ~1.4 (1.5 J over ~1.1 s)", rd.Value)
	}
}

// Docker's powercap mask can survive a bind-mount at the canonical path, so
// the first root may be empty while the host has RAPL: the sensor must find
// zones at the next configured root (docker-compose's /powercap fallback).
func TestCPUPowerFallbackRAPLRoot(t *testing.T) {
	rapl := fakePowercapTree(t, "1000000")
	s := CPUPower([]string{t.TempDir(), rapl}, fakeHWmonTree(t), nil, nil, 4)
	rd, err := s.Sample()
	if err != nil {
		t.Fatalf("second root has zones; hwmon fallback must not be used: %v", err)
	}
	if rd.Value != 0 {
		t.Fatalf("first sample = %v, want 0 (no previous counters)", rd.Value)
	}
}

// Sub-zones (intel-rapl:N:M) report energy that is already inside the
// parent top-level zone (intel-rapl:N); summing both double counts. Zone
// dir names contain ':' which NTFS forbids, so this runs on Linux hosts
// only — the production target.
func TestCPUPowerSkipsSubZones(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("RAPL zone dir names contain ':', uncreatable on NTFS")
	}
	root := t.TempDir()
	parent := filepath.Join(root, "intel-rapl:0")
	sub := filepath.Join(root, "intel-rapl:0:1")
	for _, d := range []string{parent, sub} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "max_energy_range_uj"), []byte("10000000"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(parent, "energy_uj"), []byte("1000000"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "energy_uj"), []byte("1000000"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := CPUPower([]string{root}, t.TempDir(), nil, nil, 4)
	if _, err := s.Sample(); err != nil {
		t.Fatal(err)
	}
	// Parent +2 J, sub-zone +0.5 J (a subset of the parent's energy).
	if err := os.WriteFile(filepath.Join(parent, "energy_uj"), []byte("3000000"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "energy_uj"), []byte("1500000"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	rd, err := s.Sample()
	if err != nil {
		t.Fatal(err)
	}
	// ~1.8 W when only the parent counts; summing both gives ~2.3 W.
	if rd.Value < 1.3 || rd.Value > 2.1 {
		t.Fatalf("Value = %v W, want ~1.8 (sub-zone must not be summed)", rd.Value)
	}
}

// Regression: Fahrenheit converted the ABSENT sibling max too (0 °C ->
// 32 °F), which then satisfied the "sibMax > 0" case and set Max=32 <
// Min=70 — the 230 °F default was unreachable.
func TestHwmonTempFahrenheitDefaultMax(t *testing.T) {
	s := HwmonTemp(fakeHWmonTree(t), TempOptions{Chips: []string{"coretemp"}, Labels: []string{"NoSuchLabel"}, Fahrenheit: true, MaxPoints: 1})
	rd, err := s.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if rd.Value != 95 { // temp1_input fallback: 35 °C -> 95 °F
		t.Fatalf("Value = %v, want 95 °F", rd.Value)
	}
	if rd.Max != 230 || rd.Min != 70 {
		t.Fatalf("Min/Max = %v/%v, want 70/230 (default Fahrenheit scale)", rd.Min, rd.Max)
	}
}

// The plain *_input fallback must walk the chip list in priority order:
// with the default chips (coretemp first, nvme last) the old lexicographic
// hwmonN scan handed nvme's power1_input to cpu_power.
func TestCPUPowerHwmonFallbackChipPriority(t *testing.T) {
	root := t.TempDir()
	writeHWmon(t, root, "hwmon0", "nvme", map[string]string{"power1_label": "Power 1", "power1_input": "5000000"})
	writeHWmon(t, root, "hwmon1", "coretemp", map[string]string{"power1_input": "15000000"})
	s := CPUPower([]string{t.TempDir()}, root, []string{"coretemp", "nvme"}, []string{"NoSuchPowerLabel"}, 1)
	rd, err := s.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if rd.Value != 15 {
		t.Fatalf("Value = %v W, want 15 (coretemp outranks nvme by chip priority)", rd.Value)
	}
}

func TestCPUPowerHwmonFallback(t *testing.T) {
	// No RAPL zones: the Go port additionally reads hwmon power*_input µW
	// (extension; the original reported 0 W).
	s := CPUPower([]string{t.TempDir()}, fakeHWmonTree(t), []string{"coretemp"}, []string{"Package power"}, 1)
	rd, err := s.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if rd.Value != 15 {
		t.Fatalf("Value = %v, want 15 W (15000000 µW)", rd.Value)
	}
}
