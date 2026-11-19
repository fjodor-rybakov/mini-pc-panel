package metrics

import (
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"
	"time"

	gcpu "github.com/shirou/gopsutil/v4/cpu"
	gdisk "github.com/shirou/gopsutil/v4/disk"
	gmem "github.com/shirou/gopsutil/v4/mem"
	gnet "github.com/shirou/gopsutil/v4/net"
)

// joinSeries formats every history value and joins with commas exactly like
// Array.join() on the already-toFixed strings the original sensors pushed
// (the original widgets received chart data as this joined format token).
func joinSeries(vals []float64, format string) string {
	out := make([]string, len(vals))
	for i, v := range vals {
		out[i] = fmt.Sprintf(format, v)
	}
	return strings.Join(out, ",")
}

// prefilledRing mirrors the original sensors, which pre-fill their history
// arrays with max_points zeros before pushing (charts are always full).
func prefilledRing(size int) *ring {
	r := newRing(size)
	for i := 0; i < r.size; i++ {
		r.push(0)
	}
	return r
}

func ringMax(vals []float64) float64 {
	m := 0.0
	for _, v := range vals {
		if v > m {
			m = v
		}
	}
	return m
}

// ---- clock (s1panel/sensors/clock.js) ----

type clockSensor struct{}

// Clock returns a Sensor named "clock" (min 0, max 0 like the original).
// Format tokens: {0} H:MM (24h, hour not padded), {1} H:MM (12h),
// {2} SS, {3} am/pm.
func Clock() Sensor { return &clockSensor{} }

func (clockSensor) Name() string { return "clock" }

func (clockSensor) Sample() (Reading, error) {
	now := time.Now()
	h, m, s := now.Hour(), now.Minute(), now.Second()
	h12 := h
	ampm := "am"
	if h >= 12 {
		ampm = "pm"
		if h > 12 {
			h12 = h - 12
		}
	} else if h == 0 {
		h12 = 12
	}
	return Reading{
		Value: float64(h*3600 + m*60 + s),
		Min:   0,
		Max:   0,
		Text: []string{
			fmt.Sprintf("%d:%02d", h, m),
			fmt.Sprintf("%d:%02d", h12, m),
			fmt.Sprintf("%02d", s),
			ampm,
		},
		When: now,
	}, nil
}

// ---- calendar (s1panel/sensors/calendar.js) ----

type calendarSensor struct{}

// Calendar returns a Sensor named "calendar" (min 0, max 0 like the
// original). Format tokens: {0} M/D/YY, {1} YYYY-M-D, {2} day of month,
// {3} ordinal (1st/2nd/3rd/4th), {4} weekday full, {5} weekday 3-char,
// {6} month full, {7} month 3-char, {8} 4-digit year, {9} month number,
// {10} 2-digit year, {11} YYYY.M.D.
func Calendar() Sensor { return &calendarSensor{} }

func (calendarSensor) Name() string { return "calendar" }

var weekdayNames = [...]string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
var monthNames = [...]string{"January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"}

// ordinalSuffix reproduces get_ordinal_suffix() from calendar.js.
func ordinalSuffix(n int) string {
	if n >= 11 && n <= 13 {
		return "th"
	}
	switch n % 10 {
	case 1:
		return "st"
	case 2:
		return "nd"
	case 3:
		return "rd"
	default:
		return "th"
	}
}

func (calendarSensor) Sample() (Reading, error) {
	now := time.Now()
	month := int(now.Month())
	day := now.Day()
	year := now.Year()
	shortYear := year % 100
	wd := int(now.Weekday())
	mo := month - 1
	return Reading{
		Value: float64(year*10000 + month*100 + day),
		Min:   0,
		Max:   0,
		Text: []string{
			fmt.Sprintf("%d/%d/%02d", month, day, shortYear),
			fmt.Sprintf("%d-%d-%d", year, month, day),
			strconv.Itoa(day),
			strconv.Itoa(day) + ordinalSuffix(day),
			weekdayNames[wd],
			weekdayNames[wd][:3],
			monthNames[mo],
			monthNames[mo][:3],
			strconv.Itoa(year),
			strconv.Itoa(month),
			fmt.Sprintf("%02d", shortYear),
			fmt.Sprintf("%d.%d.%d", year, month, day),
		},
		When: now,
	}, nil
}

// ---- cpu_usage (s1panel/sensors/cpu_usage.js) ----

type cpuUsageSensor struct{ hist *ring }

// CPUUsage returns a Sensor named "cpu_usage": aggregate CPU usage percent.
// History stores integer-rounded samples like the original.
// Format tokens: {0} last usage, {1} history joined (max 100).
func CPUUsage(maxPoints int) Sensor { return &cpuUsageSensor{hist: prefilledRing(maxPoints)} }

func (cpuUsageSensor) Name() string { return "cpu_usage" }

func (s *cpuUsageSensor) Sample() (Reading, error) {
	// interval 0 = delta against the previous call (steady ticker makes this
	// equivalent to the original /proc/stat two-read delta)
	p, err := gcpu.Percent(0, false)
	if err != nil {
		return Reading{}, fmt.Errorf("cpu_usage: %w", err)
	}
	v := 0.0
	if len(p) > 0 {
		v = p[0]
	}
	s.hist.push(v)
	vals := s.hist.values()
	return Reading{
		Value: v, Min: 0, Max: 100,
		Text:    []string{fmt.Sprintf("%.0f", v), joinSeries(vals, "%.0f")},
		History: vals,
		Series:  map[int]Series{1: {Values: vals, Max: 100}},
		When:    time.Now(),
	}, nil
}

// ---- memory (s1panel/sensors/memory.js) ----

type memorySensor struct {
	freeR, cacheR, activeR, usageR, swapUsedR, swapR *ring
}

// Memory returns a Sensor named "memory". Six histories are kept (free,
// cached, active bytes; usage and swap percent; swap used bytes), all
// pre-filled with zeros like the original. Format tokens:
// {0} total bytes, {1} swap total bytes,
// {2} usage % (max 100), {3} usage history joined (max 100),
// {4} swap % (max 100), {5} swap history joined (max 100),
// {6} free bytes (max total), {7} free history joined (max total),
// {8} cached bytes (max total), {9} cached history joined (max total),
// {10} active bytes (max total), {11} active history joined (max total),
// {12} swap used bytes (max swap total), {13} swap used joined (max swap total),
// {14} format_bytes(active), {15} format_bytes(swap used).
func Memory(maxPoints int) Sensor {
	return &memorySensor{
		freeR: prefilledRing(maxPoints), cacheR: prefilledRing(maxPoints),
		activeR: prefilledRing(maxPoints), usageR: prefilledRing(maxPoints),
		swapUsedR: prefilledRing(maxPoints), swapR: prefilledRing(maxPoints),
	}
}

func (memorySensor) Name() string { return "memory" }

func (s *memorySensor) Sample() (Reading, error) {
	v, err := gmem.VirtualMemory()
	if err != nil {
		return Reading{}, fmt.Errorf("memory: %w", err)
	}
	sw, err := gmem.SwapMemory()
	if err != nil {
		return Reading{}, fmt.Errorf("memory swap: %w", err)
	}
	// The original matched only the "Cached:" line of /proc/meminfo;
	// gopsutil's Cached additionally includes SReclaimable.
	cached := v.Cached
	if v.Sreclaimable <= cached {
		cached -= v.Sreclaimable
	}
	used := v.Total - v.Free
	active := used - cached
	usagePct := 0.0
	if v.Total > 0 {
		usagePct = float64(active) / float64(v.Total) * 100
	}
	swapUsed := sw.Total - sw.Free
	swapPct := 0.0
	if sw.Total > 0 {
		swapPct = float64(swapUsed) / float64(sw.Total) * 100
	}
	s.freeR.push(float64(v.Free))
	s.cacheR.push(float64(cached))
	s.activeR.push(float64(active))
	s.usageR.push(usagePct)
	s.swapUsedR.push(float64(swapUsed))
	s.swapR.push(swapPct)

	freeV, cacheV, activeV := s.freeR.values(), s.cacheR.values(), s.activeR.values()
	usageV, swapUsedV, swapV := s.usageR.values(), s.swapUsedR.values(), s.swapR.values()
	total := float64(v.Total)
	swapTotal := float64(sw.Total)

	return Reading{
		Value: usagePct, Min: 0, Max: 100,
		Text: []string{
			strconv.FormatUint(v.Total, 10),
			strconv.FormatUint(sw.Total, 10),
			fmt.Sprintf("%.2f", usagePct),
			joinSeries(usageV, "%.2f"),
			fmt.Sprintf("%.2f", swapPct),
			joinSeries(swapV, "%.2f"),
			strconv.FormatUint(v.Free, 10),
			joinSeries(freeV, "%.0f"),
			strconv.FormatUint(cached, 10),
			joinSeries(cacheV, "%.0f"),
			strconv.FormatUint(active, 10),
			joinSeries(activeV, "%.0f"),
			strconv.FormatUint(swapUsed, 10),
			joinSeries(swapUsedV, "%.0f"),
			formatBytes(float64(active)),
			formatBytes(float64(swapUsed)),
		},
		History: usageV,
		Series: map[int]Series{
			3:  {Values: usageV, Max: 100},
			5:  {Values: swapV, Max: 100},
			7:  {Values: freeV, Max: total},
			9:  {Values: cacheV, Max: total},
			11: {Values: activeV, Max: total},
			13: {Values: swapUsedV, Max: swapTotal},
		},
		When: time.Now(),
	}, nil
}

// formatBytes reproduces format_bytes() from memory.js: GB/MB/KB ladder
// with two decimals, plain bytes otherwise.
func formatBytes(bytes float64) string {
	kb := bytes / 1024
	mb := kb / 1024
	gb := mb / 1024
	switch {
	case gb >= 1:
		return fmt.Sprintf("%.2f GB", gb)
	case mb >= 1:
		return fmt.Sprintf("%.2f MB", mb)
	case kb >= 1:
		return fmt.Sprintf("%.2f KB", kb)
	default:
		return fmt.Sprintf("%.0f B", bytes)
	}
}

// ---- space (s1panel/sensors/space.js) ----

type spaceSensor struct {
	mount, name  string
	freeR, usedR *ring
}

// Space returns a Sensor named "space_<name>" for one mount point, where
// <name> is the mount point itself ("root" for "/") like the original
// (which also allowed an explicit config.name override; the Go config
// lists mount_points directly).
// Format tokens: {0} mount point, {1} free % joined, {2} used % joined,
// {3} last free %, {4} last used %, {5} free MiB, {6} used MiB,
// {7} total MiB, {8} "MiB". MB units are bytes/1048576, precision 0.
func Space(mount string, maxPoints int) Sensor {
	name := mount
	if mount == "/" {
		name = "root"
	}
	return &spaceSensor{mount: mount, name: "space_" + name, freeR: prefilledRing(maxPoints), usedR: prefilledRing(maxPoints)}
}

func (s *spaceSensor) Name() string { return s.name }

func (s *spaceSensor) Sample() (Reading, error) {
	u, err := gdisk.Usage(s.mount)
	if err != nil {
		return Reading{}, fmt.Errorf("space %s: %w", s.mount, err)
	}
	// The original used fs.statfs: total = blocks*bsize, free = bfree*bsize,
	// used = total - free (bfree, not bavail — gopsutil's Free matches).
	total := float64(u.Total)
	free := float64(u.Free)
	used := total - free
	freePct, usedPct := 0.0, 0.0
	sizeMB := 0.0
	if total > 0 {
		freePct = free / total * 100
		usedPct = used / total * 100
		sizeMB = total / 1048576
	}
	s.freeR.push(freePct)
	s.usedR.push(usedPct)
	freeV, usedV := s.freeR.values(), s.usedR.values()

	return Reading{
		Value: usedPct, Min: 0, Max: 100,
		Text: []string{
			s.mount,
			joinSeries(freeV, "%.0f"),
			joinSeries(usedV, "%.0f"),
			fmt.Sprintf("%.0f", freePct),
			fmt.Sprintf("%.0f", usedPct),
			fmt.Sprintf("%.0f", free/1048576),
			fmt.Sprintf("%.0f", used/1048576),
			fmt.Sprintf("%.0f", sizeMB),
			"MiB",
		},
		History: usedV,
		Series: map[int]Series{
			1: {Values: freeV, Max: 100},
			2: {Values: usedV, Max: 100},
		},
		When: time.Now(),
	}, nil
}

// ---- network (s1panel/sensors/network.js + network_thread.js) ----

type networkSensor struct {
	iface, name                                string
	scaling                                    float64
	sysfsRoot                                  string // /sys/class/net (overridable for tests)
	rxBytesR, txBytesR, rxPacketsR, txPacketsR *ring
	lastRx, lastTx, lastRxP, lastTxP           uint64
	haveLast                                   bool
	linkSpeed, linkMTU                         float64
	ipv4, ipv6                                 string
}

// Network returns a Sensor named "network_<iface>" tracking RAW per-sample
// deltas of the interface counters (bytes and packets, rx and tx) exactly
// like the original worker thread. sysfsRoot supplies <iface>/speed and
// <iface>/mtu (defaults 1000 / 1500 when unreadable). scaling is the
// chart headroom factor (original default 1.5).
// Format tokens (the _max side effects of the original are bound per
// token in Reading.Series):
// {0} iface, {1} last rx bytes delta, {2} rx bytes joined,
// {3} rx human data rate, {4} last tx, {5} tx joined, {6} tx human rate,
// {7} last rx packets, {8} rx packets "… rx/pps", {9} rx packets joined,
// {10} last tx packets, {11} "… tx/pps", {12} tx packets joined,
// {13} link speed, {14} link mtu, {15} rx bits/s, {16} tx bits/s,
// {17} ipv4, {18} ipv6.
func Network(iface string, maxPoints int, scaling float64, sysfsRoot string) Sensor {
	if sysfsRoot == "" {
		sysfsRoot = "/sys/class/net"
	}
	return &networkSensor{
		iface: iface, name: "network_" + iface, scaling: scaling, sysfsRoot: sysfsRoot,
		rxBytesR: prefilledRing(maxPoints), txBytesR: prefilledRing(maxPoints),
		rxPacketsR: prefilledRing(maxPoints), txPacketsR: prefilledRing(maxPoints),
		linkSpeed: 1000, linkMTU: 1500,
		ipv4: "n/a", ipv6: "n/a",
	}
}

func (s *networkSensor) Name() string { return s.name }

func (s *networkSensor) Sample() (Reading, error) {
	stats, err := gnet.IOCounters(true)
	if err != nil {
		return Reading{}, fmt.Errorf("network %s: %w", s.iface, err)
	}
	var curRx, curTx, curRxP, curTxP uint64
	found := false
	for _, st := range stats {
		if st.Name == s.iface {
			curRx, curTx, curRxP, curTxP = st.BytesRecv, st.BytesSent, st.PacketsRecv, st.PacketsSent
			found = true
			break
		}
	}
	if !found {
		return Reading{}, fmt.Errorf("network: interface %s not present", s.iface)
	}
	// Original: first sample yields 0 deltas (`_last ? cur - _last : 0`).
	dRx, dTx, dRxP, dTxP := 0.0, 0.0, 0.0, 0.0
	if s.haveLast {
		if curRx >= s.lastRx {
			dRx = float64(curRx - s.lastRx)
		}
		if curTx >= s.lastTx {
			dTx = float64(curTx - s.lastTx)
		}
		if curRxP >= s.lastRxP {
			dRxP = float64(curRxP - s.lastRxP)
		}
		if curTxP >= s.lastTxP {
			dTxP = float64(curTxP - s.lastTxP)
		}
	}
	s.lastRx, s.lastTx, s.lastRxP, s.lastTxP = curRx, curTx, curRxP, curTxP
	s.haveLast = true

	if sp, err := strconv.ParseFloat(readTrim(s.sysfsRoot+"/"+s.iface+"/speed"), 64); err == nil && sp > 0 {
		s.linkSpeed = sp
	}
	if m, err := strconv.ParseFloat(readTrim(s.sysfsRoot+"/"+s.iface+"/mtu"), 64); err == nil && m > 0 {
		s.linkMTU = m
	}
	s.ipv4, s.ipv6 = ifaceAddresses(s.iface)

	s.rxBytesR.push(dRx)
	s.txBytesR.push(dTx)
	s.rxPacketsR.push(dRxP)
	s.txPacketsR.push(dTxP)
	rxB, txB := s.rxBytesR.values(), s.txBytesR.values()
	rxP, txP := s.rxPacketsR.values(), s.txPacketsR.values()

	// max_link_capacity_bytes: link_speed * 0.125 * 1024 * 1024
	capacity := s.linkSpeed * 0.125 * 1024 * 1024
	scaled := func(m float64) float64 {
		if s.scaling != 0 {
			return math.Min(math.Ceil(m*s.scaling), capacity)
		}
		return capacity
	}
	rxMax, txMax := scaled(ringMax(rxB)), scaled(ringMax(txB))
	rxPMax, txPMax := ringMax(rxP), ringMax(txP)

	return Reading{
		Value: dRx, Min: 0, Max: capacity,
		Text: []string{
			s.iface,
			fmt.Sprintf("%.0f", dRx),
			joinSeries(rxB, "%.0f"),
			bytesToDataRate(dRx, false),
			fmt.Sprintf("%.0f", dTx),
			joinSeries(txB, "%.0f"),
			bytesToDataRate(dTx, false),
			fmt.Sprintf("%.0f", dRxP),
			fmt.Sprintf("%.0f rx/pps", dRxP),
			joinSeries(rxP, "%.0f"),
			fmt.Sprintf("%.0f", dTxP),
			fmt.Sprintf("%.0f tx/pps", dTxP),
			joinSeries(txP, "%.0f"),
			strconv.FormatFloat(s.linkSpeed, 'f', 0, 64),
			strconv.FormatFloat(s.linkMTU, 'f', 0, 64),
			bytesToDataRate(dRx, true),
			bytesToDataRate(dTx, true),
			s.ipv4,
			s.ipv6,
		},
		History: rxB,
		Series: map[int]Series{
			1: {Values: rxB, Max: rxMax}, 2: {Values: rxB, Max: rxMax}, 3: {Values: rxB, Max: rxMax},
			4: {Values: txB, Max: txMax}, 5: {Values: txB, Max: txMax}, 6: {Values: txB, Max: txMax},
			7: {Values: rxP, Max: rxPMax}, 8: {Values: rxP, Max: rxPMax}, 9: {Values: rxP, Max: rxPMax},
			10: {Values: txP, Max: txPMax}, 11: {Values: txP, Max: txPMax}, 12: {Values: txP, Max: txPMax},
		},
		When: time.Now(),
	}, nil
}

// ifaceAddresses picks the first non-internal IPv4/IPv6 address of an
// interface, "n/a" when absent (os.networkInterfaces find in the original).
func ifaceAddresses(iface string) (string, string) {
	ipv4, ipv6 := "n/a", "n/a"
	it, err := net.InterfaceByName(iface)
	if err != nil {
		return ipv4, ipv6
	}
	addrs, err := it.Addrs()
	if err != nil {
		return ipv4, ipv6
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipnet.IP
		if ip.To4() != nil {
			if ipv4 == "n/a" {
				ipv4 = ip.String()
			}
		} else if ipv6 == "n/a" {
			ipv6 = ip.String()
		}
	}
	return ipv4, ipv6
}

// bytesToDataRate reproduces bytes_to_data_rate() from network.js,
// including its decimal-place ladder and unit suffixes.
func bytesToDataRate(bytes float64, bits bool) string {
	v := bytes
	if bits {
		v *= 8
	}
	kb := v / 1024.0
	mb := kb / 1024.0
	gb := mb / 1024.0
	switch {
	case gb >= 1:
		return fmt.Sprintf("%.3f %s", gb, rateUnit(bits, "Gbit/s", "Gb/s"))
	case mb >= 1:
		if bits {
			return fmt.Sprintf("%.1f %s", mb, rateUnit(bits, "Mbit/s", "Mb/s"))
		}
		return fmt.Sprintf("%.2f %s", mb, rateUnit(bits, "Mbit/s", "Mb/s"))
	case kb >= 1:
		if bits {
			return fmt.Sprintf("%.0f %s", kb, rateUnit(bits, "kbit/s", "Kb/s"))
		}
		return fmt.Sprintf("%.2f %s", kb, rateUnit(bits, "kbit/s", "Kb/s"))
	default:
		return fmt.Sprintf("%.0f %s", bytes, rateUnit(bits, "bit/s", "B/s"))
	}
}

func rateUnit(bits bool, bitUnit, byteUnit string) string {
	if bits {
		return bitUnit
	}
	return byteUnit
}
