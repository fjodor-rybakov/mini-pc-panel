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

// OpenHID finds the panel (VID/PID) and prefers the configured interface
// number (MI_01 → 1, spec §2). Firmware revisions disagree: original units
// put the screen on interface 1 ("1-8:1.1" in the original config.json);
// newer units observed in the field have no interface 1 at all (ifaces 0
// and 2). When the exact match fails, sysfs decides which of the panel's
// interfaces is the screen — the one declaring a (nonzero) Output report,
// buttons/keyboard declare none — and that interface's enumeration entry
// is opened with its backend-native path, hidraw or libusb alike (spec
// §1). Note for newer firmware: its descriptor under-declares the screen
// output report (observed 512 bits for 4104-byte frames), so the kernel
// hidraw layer rejects the writes — such units need the libusb build
// (`make build-libusb`, Docker default), exactly like the original Node.js
// app did with node_hid.setDriverType('libusb').
func OpenHID(vid, pid uint16, iface int) (Transport, error) {
	if err := hid.Init(); err != nil {
		return nil, fmt.Errorf("display: hid.Init: %w", err)
	}
	ifPaths := map[int]string{}
	var seen []string
	err := hid.Enumerate(vid, pid, func(di *hid.DeviceInfo) error {
		if _, dup := ifPaths[di.InterfaceNbr]; !dup {
			ifPaths[di.InterfaceNbr] = di.Path
		}
		seen = append(seen, fmt.Sprintf("iface %d at %s", di.InterfaceNbr, di.Path))
		return nil
	})
	if err != nil {
		hid.Exit()
		return nil, fmt.Errorf("display: enumerate %04x:%04x: %w", vid, pid, err)
	}
	if path, ok := ifPaths[iface]; ok {
		return openHIDPath(path)
	}
	node, nodeIface, bits, diag := probeScreenHidraw(vid, pid)
	if bits > 0 {
		if path, ok := ifPaths[nodeIface]; ok {
			return openHIDPath(path)
		}
		if node != "" {
			return openHIDPath(node) // last resort: kernel node (hidraw builds)
		}
	}
	hid.Exit()
	if len(seen) == 0 {
		return nil, fmt.Errorf("display: no HID device %04x:%04x enumerated and sysfs found nothing either [%s] (panel not plugged in, or invisible here — check lsusb; in Docker, whether /sys is the host sysfs)", vid, pid, diag)
	}
	return nil, fmt.Errorf("display: %04x:%04x enumerated but no interface %d among [%s], and sysfs probing [%s] found no screen interface — set device.path in config.json (format is backend-specific: /dev/hidrawN for hidraw builds) and if the panel is a newer revision use the libusb build (make build-libusb)", vid, pid, iface, strings.Join(seen, ", "), diag)
}

// OpenPath opens an explicit HID path. Config escape hatch: device.path.
// The path format is backend-specific: /dev/hidrawN or a udev symlink for
// hidraw builds; libusb builds want the path hid.Enumerate reports (the
// log line "iface N at <path>" shows exactly those). Use it when neither
// the interface number nor the sysfs report-descriptor probe picks the
// screen (restricted sysfs in a container, two candidate nodes
// indistinguishable from sysfs): try each — the screen is the interface
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
// nodes and returns the screen node — the panel interface whose report
// descriptor declares the largest nonzero Output report (buttons/keyboard
// declare none) — as the /dev path, the interface number parsed from the
// sysfs device name ("<busid>:<config>.<interface>"), the bit size, and a
// diagnosis of everything seen. Works where hidapi's interface numbers are
// unusable (firmware revisions without MI_01, interface -1).
//
// Note the threshold is "nonzero", not "a full frame": newer firmware
// revisions lie in their descriptors (observed: the screen interface
// declares 512 bits while actually accepting 4104-byte frames), so among
// the panel's own interfaces the screen is simply the one with any Output
// report at all. In Docker /sys is the host sysfs, so the probe still sees
// every node.
//
// A node belongs to the panel if uevent says so (PRODUCT=/MODALIAS=) or —
// the format-proof fallback — its sysfs device directory is named
// "<bus>:<VENDOR>:<PRODUCT>.<instance>", which is how the HID core
// registers every device on every kernel, whatever the uevent file looks
// like there.
func probeScreenHidraw(vid, pid uint16) (path string, iface int, bits int, diag string) {
	const noIface = -1
	entries, err := os.ReadDir("/sys/class/hidraw")
	if err != nil {
		return "", noIface, 0, fmt.Sprintf("cannot read /sys/class/hidraw: %v", err)
	}
	idTag := fmt.Sprintf("%04X:%04X", vid, pid)
	best, bestBits, bestIface := "", 0, noIface
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
		nodeIface := parseHidIfaceFromSysfs(target)
		rd, err := os.ReadFile(dev + "/report_descriptor")
		if err != nil {
			matched = append(matched, fmt.Sprintf("%s[%04x:%04x iface %d]: report_descriptor unreadable (%v)", e.Name(), u, p, nodeIface, err))
			continue
		}
		b := maxOutputReportBits(rd)
		matched = append(matched, fmt.Sprintf("%s[%04x:%04x iface %d]: %d-bit output report (%d-byte descriptor)", e.Name(), u, p, nodeIface, b, len(rd)))
		if b > bestBits {
			best, bestBits, bestIface = "/dev/"+e.Name(), b, nodeIface
		}
	}
	switch {
	case bestBits > 0:
		return best, bestIface, bestBits, strings.Join(matched, "; ")
	case len(matched) > 0:
		return "", noIface, 0, fmt.Sprintf("panel node(s) found but none declares an output report: %s", strings.Join(matched, "; "))
	case len(entries) == 0:
		return "", noIface, 0, "/sys/class/hidraw is empty — the host sysfs is not visible to this process"
	default:
		return "", noIface, 0, fmt.Sprintf("none of %d hidraw node(s) carries %s: [%s]", len(entries), idTag, strings.Join(others, ", "))
	}
}

// Shutdown releases the HID subsystem; call once after the last Display close.
func Shutdown() { hid.Exit() }
