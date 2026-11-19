//go:build linux

package display

import (
	"fmt"
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
// When the interface cannot be matched, the error says what enumeration
// actually saw: an empty enumeration means the device is simply not visible
// here (cable/power, or a container without the host sysfs), while a
// non-empty list of other interface numbers (hidapi reports -1 when it
// cannot parse them from sysfs) points to the explicit override device.path
// in config.json — see OpenPath.
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
		hid.Exit()
		if len(seen) == 0 {
			return nil, fmt.Errorf("display: no HID device %04x:%04x enumerated at all (not plugged in, or not visible to this process — check lsusb; in Docker, whether /sys is the host sysfs)", vid, pid)
		}
		return nil, fmt.Errorf("display: %04x:%04x enumerated but none of its interfaces is %d [found: %s] — set device.path in config.json to the /dev/hidrawN whose sysfs device path ends in :1.1 (the MI_01 node)", vid, pid, iface, strings.Join(seen, ", "))
	}
	return openHIDPath(path)
}

// OpenPath opens an explicit HID device node — /dev/hidrawN or a udev
// symlink such as /dev/s1panel-hidraw. Config escape hatch: device.path.
// Use it when enumeration cannot tell MI_00 from MI_01 (interface -1 or a
// restricted sysfs in a container); the node is picked once, by hand, with
// `readlink -f /sys/class/hidraw/hidrawN/device` ending in ":1.1".
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

// Shutdown releases the HID subsystem; call once after the last Display close.
func Shutdown() { hid.Exit() }
