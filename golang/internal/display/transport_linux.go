//go:build linux

package display

import (
	"fmt"

	"github.com/sstallion/go-hid"
)

type hidTransport struct {
	dev *hid.Device
}

// OpenHID finds the panel (VID/PID) and opens strictly the MI_01 interface
// (InterfaceNbr == iface, spec §2). Builds with the default hidraw backend;
// with `-tags libusb` go-hid switches to libusb without code changes
// (spec §1). Requires cgo against -ludev (hidraw) or -lusb-1.0 (libusb).
func OpenHID(vid, pid uint16, iface int) (Transport, error) {
	if err := hid.Init(); err != nil {
		return nil, fmt.Errorf("display: hid.Init: %w", err)
	}
	var path string
	err := hid.Enumerate(vid, pid, func(di *hid.DeviceInfo) error {
		if path == "" && di.InterfaceNbr == iface {
			path = di.Path
		}
		return nil
	})
	if err != nil {
		hid.Exit()
		return nil, fmt.Errorf("display: enumerate %04x:%04x: %w", vid, pid, err)
	}
	if path == "" {
		hid.Exit()
		return nil, fmt.Errorf("display: no HID interface %d found for %04x:%04x (check udev rules)", iface, vid, pid)
	}
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
