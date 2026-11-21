// Package led drives the S1 panel LED strip through its CH340 USB-UART link
// (doc/revrite.md §4): 5-byte packets [0xFA, theme, intensity, speed, checksum].
package led

import (
	"fmt"
	"io"
	"time"

	"go.bug.st/serial"
)

// Protocol constants (spec §4).
const (
	Signature = 0xFA

	ThemeRainbow   = 0x01
	ThemeBreathing = 0x02
	ThemeColors    = 0x03
	ThemeOff       = 0x04
	ThemeAuto      = 0x05
	ThemeIgnore    = 0x06 // led_thread.js "case 6: // ignore" - no packet is sent

	MinValue = 1 // max intensity / fastest speed on the wire
	MaxValue = 5 // min intensity / slowest speed on the wire

	// BaudRate matches open_device in the original led_device.js (10000).
	BaudRate = 10000
)

// FixValue converts config/GUI values to protocol values exactly like
// fix_value() in led_device.js: the user writes 1 = dim/slow .. 5 =
// bright/fast, the controller expects 0x01 = max .. 0x05 = min, and
// min(5, max(6-num, 1)) performs the inversion with clamping.
func FixValue(num int) int {
	return min(MaxValue, max(6-num, MinValue))
}

// Packet builds the 5-byte command for the themed set_* commands, applying
// fix_value() to intensity and speed exactly as set_rainbow(),
// set_breathing(), set_color() and set_automatic() in led_device.js do.
// The intensity/speed arguments are config-scale values (1 = dim/slow ..
// 5 = bright/fast).
func Packet(theme, intensity, speed int) []byte {
	return packet(theme, FixValue(intensity), FixValue(speed))
}

// OffPacket mirrors set_off(): FA 04 05 05 with hard-coded protocol 5/5 -
// the original never routes set_off through fix_value().
func OffPacket() []byte { return packet(ThemeOff, MaxValue, MaxValue) }

// packet builds the frame from protocol-scale values; checksum is the uint8
// sum of the first four bytes, exactly like checksum() in led_device.js.
func packet(theme, protocolIntensity, protocolSpeed int) []byte {
	t, i, s := clampTheme(theme), clamp(protocolIntensity), clamp(protocolSpeed)
	p := []byte{Signature, byte(t), byte(i), byte(s), 0x00}
	sum := uint16(p[0]) + uint16(p[1]) + uint16(p[2]) + uint16(p[3])
	p[4] = byte(sum & 0xFF)
	return p
}

// clampTheme keeps the wire theme within 1..5: the original switch has no
// case outside that range (case 6 never reaches packet building).
func clampTheme(v int) int {
	if v < ThemeRainbow {
		return ThemeRainbow
	}
	if v > ThemeAuto {
		return ThemeAuto
	}
	return v
}

func clamp(v int) int {
	if v < MinValue {
		return MinValue
	}
	if v > MaxValue {
		return MaxValue
	}
	return v
}

// byteDelay mirrors DELAY=5 in led_device.js: the original sent the frame
// one byte at a time with a 5 ms pause between writes (port_byte_write +
// setTimeout), i.e. ~4 ms of idle line between bytes at 10000 baud. The
// HT32-side LED parser needs those idle gaps; a back-to-back burst is
// dropped and the strip keeps whatever mode it already had - including
// staying lit after "off".
const byteDelay = 5 * time.Millisecond

// frameSink is the part of serial.Port used to push one frame; tests
// substitute a fake to observe the pacing without hardware.
type frameSink interface {
	io.Writer
	Drain() error
}

// writeFrame reproduces port_byte_write(): one Write per byte, byteDelay
// apart, then Drain so the last byte has physically left before Close.
func writeFrame(s frameSink, frame []byte, sleep func(time.Duration)) error {
	for i, b := range frame {
		if i > 0 {
			sleep(byteDelay)
		}
		if _, err := s.Write([]byte{b}); err != nil {
			return fmt.Errorf("led: write to port: %w", err)
		}
	}
	return s.Drain()
}

// Device writes LED commands to the CH340 serial port.
type Device struct {
	port serial.Port
}

// Open attaches to the serial port (e.g. "/dev/ttyUSB0") at 10000 baud 8N1.
// go.bug.st/serial compiles everywhere; Open only succeeds where the device
// exists, so unit tests exercise Packet() without hardware.
func Open(port string) (*Device, error) {
	mode := &serial.Mode{BaudRate: BaudRate, DataBits: 8, Parity: serial.NoParity, StopBits: serial.OneStopBit}
	p, err := serial.Open(port, mode) // v1.8 applies the mode during open
	if err != nil {
		return nil, fmt.Errorf("led: open %s: %w", port, err)
	}
	return &Device{port: p}, nil
}

// Set applies theme/intensity/speed (config scale; the fix_value() inversion
// happens inside, like the original set_* functions). Theme 6 is the
// original "ignore" case: no packet is written.
func (d *Device) Set(theme, intensity, speed int) error {
	if theme == ThemeIgnore {
		return nil // original case 6 never reaches the port at all
	}
	if d == nil || d.port == nil {
		return nil
	}
	return writeFrame(d.port, Packet(theme, intensity, speed), time.Sleep)
}

// Off turns the strip off with the hard-coded set_off() pattern.
func (d *Device) Off() error {
	if d == nil || d.port == nil {
		return nil
	}
	return writeFrame(d.port, OffPacket(), time.Sleep)
}

// Close releases the serial port.
func (d *Device) Close() error {
	if d == nil || d.port == nil {
		return nil
	}
	return d.port.Close()
}
