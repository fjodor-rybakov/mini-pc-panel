//go:build linux

package display

import (
	"fmt"
	"os"
	"strings"

	"github.com/sstallion/go-hid"
)

type hidTransport struct {
	dev *hid.Device
}

// OpenHID finds the panel (VID/PID) and opens strictly the MI_01 interface
// (InterfaceNbr == iface, spec §2). Builds with the default hidraw backend;
// with `-tags libusb` go-hid switches to libusb without code changes
// (spec §1). Requires cgo against -ludev (hidraw) or -lusb-1.0 (libusb).
//
// Firmware revisions differ: the original units put the screen on MI_01
// (:1.1 — the original config.json used "1-8:1.1"), newer ones skip MI_01
// entirely (hidraw nodes at :1.0 and :1.2), and hidapi can report
// interface -1 where it cannot parse sysfs. So when the configured
// interface is absent, the panel's hidraw nodes are probed via sysfs by
// report descriptor (probeScreenHidraw): only the screen has the
// 4104-byte output report. If that fails too, the error lists what
// enumeration saw so the node can be pinned with device.path — see OpenPath.
func OpenHID(vid, pid uint16, iface int) (Transport, error) {
	if err := hid.Init(); err != nil {
		return nil, fmt.Errorf("display: hid.Init: %w", err)
	}
	var path string
	var seen []string
	err := hid.Enumerate(vid, pid, func(di *hid.DeviceInfo) error {
		if path == "" && di.InterfaceNbr == iface {
			path = di.Path
		}
		seen = append(seen, fmt.Sprintf("iface %d at %s", di.InterfaceNbr, di.Path))
		return nil
	})
	if err != nil {
		hid.Exit()
		return nil, fmt.Errorf("display: enumerate %04x:%04x: %w", vid, pid, err)
	}
	if path == "" {
		node, diag := probeScreenHidraw(vid, pid)
		if node != "" {
			return openHIDPath(node)
		}
		hid.Exit()
		if diag == "" {
			diag = fmt.Sprintf("no hidraw node with %04x:%04x in its uevent/modalias under /sys/class/hidraw", vid, pid)
		}
		if len(seen) == 0 {
			return nil, fmt.Errorf("display: no HID device %04x:%04x enumerated and sysfs found no screen node either [%s] (panel not plugged in, or invisible here — check lsusb; in Docker, whether /sys is the host sysfs)", vid, pid, diag)
		}
		return nil, fmt.Errorf("display: %04x:%04x enumerated but no interface %d among [%s], and sysfs probing [%s] found no hidraw node with a %d-byte output report — set device.path in config.json, trying each of the panel's /dev/hidrawN in turn", vid, pid, iface, strings.Join(seen, ", "), diag, PacketSize)
	}
	return openHIDPath(path)
}

// OpenPath opens an explicit HID device node — /dev/hidrawN or a udev
// symlink. Config escape hatch: device.path. Use it when neither the
// interface number nor the sysfs report-descriptor probe picks the screen
// (restricted sysfs in a container, two candidate nodes indistinguishable
// from sysfs): try each of the panel's /dev/hidrawN — the screen is the one
// that comes alive.
func OpenPath(path string) (Transport, error) {
	if err := hid.Init(); err != nil {
		return nil, fmt.Errorf("display: hid.Init: %w", err)
	}
	return openHIDPath(path)
}

func openHIDPath(path string) (Transport, error) {
	dev, err := hid.OpenPath(path)
	if err != nil {
		hid.Exit()
		return nil, fmt.Errorf("display: open %s: %w", path, err)
	}
	return &hidTransport{dev: dev}, nil
}

// Write sends one 4104-byte packet with the leading 0x00 Report ID byte
// prepended, per the spec §2 note (4105 bytes reach dev.Write).
func (t *hidTransport) Write(packet []byte) error {
	if len(packet) != PacketSize {
		return fmt.Errorf("display: packet must be %d bytes, got %d", PacketSize, len(packet))
	}
	buf := make([]byte, 0, PacketSize+1)
	buf = append(buf, 0x00)
	buf = append(buf, packet...)
	_, err := t.dev.Write(buf)
	return err
}

func (t *hidTransport) Close() error { return t.dev.Close() }

// probeScreenHidraw scans the read-only host sysfs for the panel's hidraw
// nodes and returns the /dev path of the one whose report descriptor
// declares an Output report of at least a full frame (PacketSize bytes) —
// only the screen interface has one; buttons/keyboard never do. Works where
// hidapi's interface numbers are unusable (firmware revisions without MI_01,
// interface -1). In Docker /sys is the host sysfs, so the probe still sees
// every node; opening one that was not passed with --device then fails with
// a clear display: open error.
//
// A node belongs to the panel if uevent says so (PRODUCT=/MODALIAS=) or —
// the format-proof fallback — its sysfs device directory is named
// "<bus>:<VENDOR>:<PRODUCT>.<instance>", which is how the HID core
// registers every device on every kernel, whatever the uevent file looks
// like there.
//
// The diag return accounts for everything the probe saw: matched nodes with
// their output-report sizes, or — when nothing matched — every hidraw node
// with the sysfs name the kernel gave it. A failure in the log then states
// WHY: empty sysfs (host sysfs invisible to the process), unexpected uevent
// format (the node names expose it), or unreadable report_descriptor.
func probeScreenHidraw(vid, pid uint16) (path, diag string) {
	const minScreenBits = PacketSize * 8
	entries, err := os.ReadDir("/sys/class/hidraw")
	if err != nil {
		return "", fmt.Sprintf("cannot read /sys/class/hidraw: %v", err)
	}
	idTag := fmt.Sprintf("%04X:%04X", vid, pid)
	best, bestBits := "", 0
	var matched, others []string
	for _, e := range entries {
		dev := "/sys/class/hidraw/" + e.Name() + "/device"
		target, _ := os.Readlink(dev) // e.g. ../../…/usb1/1-2/1-2:1.2/0003:04D9:FD01.0003
		label := lastElem(target)
		if label == "" {
			label = "?"
		}
		uevent, err := os.ReadFile(dev + "/uevent")
		if err != nil {
			others = append(others, fmt.Sprintf("%s(%s, uevent: %v)", e.Name(), label, err))
			continue
		}
		u, p, ok := matchProductUEvent(string(uevent))
		if !ok && strings.Contains(target, idTag) {
			u, p, ok = vid, pid, true
		}
		if !ok {
			others = append(others, fmt.Sprintf("%s(%s)", e.Name(), label))
			continue
		}
		rd, err := os.ReadFile(dev + "/report_descriptor")
		if err != nil {
			matched = append(matched, fmt.Sprintf("%s[%04x:%04x]: report_descriptor unreadable (%v)", e.Name(), u, p, err))
			continue
		}
		bits := maxOutputReportBits(rd)
		matched = append(matched, fmt.Sprintf("%s[%04x:%04x]: %d-bit output report (%d-byte descriptor)", e.Name(), u, p, bits, len(rd)))
		if bits >= minScreenBits && bits > bestBits {
			best, bestBits = "/dev/"+e.Name(), bits
		}
	}
	switch {
	case best != "":
		return best, strings.Join(matched, "; ")
	case len(matched) > 0:
		return "", strings.Join(matched, "; ")
	case len(entries) == 0:
		return "", "/sys/class/hidraw is empty — the host sysfs is not visible to this process"
	default:
		return "", fmt.Sprintf("none of %d hidraw node(s) carries %s: [%s]", len(entries), idTag, strings.Join(others, ", "))
	}
}

// Shutdown releases the HID subsystem; call once after the last Display close.
func Shutdown() { hid.Exit() }
