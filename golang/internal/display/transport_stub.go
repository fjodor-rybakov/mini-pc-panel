//go:build !linux

package display

import "fmt"

// OpenHID is unavailable off Linux: go-hid does not compile on other
// platforms. The rest of the module (protocol, queue, render, metrics, led,
// admin server) builds and is unit-tested cross-platform, including the
// Windows development machine this code is written on.
func OpenHID(vid, pid uint16, iface int) (Transport, error) {
	return nil, fmt.Errorf("display: HID transport is only supported on Linux (VID %04x PID %04x)", vid, pid)
}

// Shutdown is a no-op on non-Linux platforms.
func Shutdown() {}
