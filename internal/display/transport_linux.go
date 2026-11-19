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
		if node := probeScreenHidraw(vid, pid); node != "" {
			return openHIDPath(node)
		}
		hid.Exit()
		if len(seen) == 0 {
			return nil, fmt.Errorf("display: no HID device %04x:%04x enumerated and sysfs probing found no hidraw node with a %d-byte output report (panel not plugged in, or invisible here — check lsusb; in Docker, whether /sys is the host sysfs)", vid, pid, PacketSize)
		}
		return nil, fmt.Errorf("display: %04x:%04x enumerated but no interface %d among [%s], and sysfs report-descriptor probing found no screen node either — set device.path in config.json, trying each of the panel's /dev/hidrawN in turn", vid, pid, iface, strings.Join(seen, ", "))
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
func probeScreenHidraw(vid, pid uint16) string {
	const minScreenBits = PacketSize * 8
	entries, err := os.ReadDir("/sys/class/hidraw")
	if err != nil {
		return ""
	}
	best, bestBits := "", 0
	for _, e := range entries {
		dir := "/sys/class/hidraw/" + e.Name() + "/device/"
		uevent, err := os.ReadFile(dir + "uevent")
		if err != nil {
			continue
		}
		u, p, ok := matchProductUEvent(string(uevent))
		if !ok || u != vid || p != pid {
			continue
		}
		rd, err := os.ReadFile(dir + "report_descriptor")
		if err != nil {
			continue
		}
		if bits := maxOutputReportBits(rd); bits >= minScreenBits && bits > bestBits {
			best, bestBits = "/dev/"+e.Name(), bits
		}
	}
	return best
}

// Shutdown releases the HID subsystem; call once after the last Display close.
func Shutdown() { hid.Exit() }
