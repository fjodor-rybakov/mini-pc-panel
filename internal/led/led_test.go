package led

import (
	"bytes"
	"testing"
)

// set_* in led_device.js runs intensity/speed through fix_value() before
// building the packet, so config values are user-scale (1 = dim/slow ..
// 5 = bright/fast) and the wire carries the inverted protocol scale.
func TestPacketBytes(t *testing.T) {
	cases := []struct {
		theme, intensity, speed int
		want                    []byte
	}{
		{ThemeRainbow, 3, 3, []byte{0xFA, 0x01, 0x03, 0x03, 0x01}},   // middle maps to middle
		{ThemeBreathing, 1, 5, []byte{0xFA, 0x02, 0x05, 0x01, 0x02}}, // fix_value() inverts
		{ThemeAuto, 5, 1, []byte{0xFA, 0x05, 0x01, 0x05, 0x05}},
	}
	for _, c := range cases {
		got := Packet(c.theme, c.intensity, c.speed)
		if !bytes.Equal(got, c.want) {
			t.Errorf("Packet(%d,%d,%d) = % X, want % X", c.theme, c.intensity, c.speed, got, c.want)
		}
	}
}

// fix_value(num) = min(5, max(6-num, 1)) from led_device.js line 39-42.
func TestFixValue(t *testing.T) {
	cases := map[int]int{0: 5, 1: 5, 2: 4, 3: 3, 4: 2, 5: 1, 6: 1, 9: 1}
	for in, want := range cases {
		if got := FixValue(in); got != want {
			t.Errorf("FixValue(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestPacketClamps(t *testing.T) {
	// Theme is clamped to the 1..5 range the original switch handles;
	// intensity/speed clamp inside fix_value().
	if got := Packet(0, 0, 0); !bytes.Equal(got, []byte{0xFA, 0x01, 0x05, 0x05, 0x05}) {
		t.Errorf("low clamp: % X", got)
	}
	if got := Packet(9, 9, 9); !bytes.Equal(got, []byte{0xFA, 0x05, 0x01, 0x01, 0x01}) {
		t.Errorf("high clamp: % X", got)
	}
}

// set_off() is hard-coded (led_device.js line 223-224): FA 04 05 05 with
// protocol-scale 5/5, NOT routed through fix_value() - so it differs from
// what Packet() would produce for the same config numbers.
func TestOffPacketMatchesSetOff(t *testing.T) {
	want := []byte{0xFA, 0x04, 0x05, 0x05, 0x08}
	if got := OffPacket(); !bytes.Equal(got, want) {
		t.Fatalf("OffPacket() = % X, want % X", got, want)
	}
	if bytes.Equal(OffPacket(), Packet(ThemeOff, MaxValue, MaxValue)) {
		t.Error("OffPacket must bypass fix_value() like set_off() does")
	}
}

// Theme 6 is led_thread.js "case 6: // ignore": Set returns before the
// serial port is ever touched, even for a nil device.
func TestThemeIgnoreNeverTouchesPort(t *testing.T) {
	var d *Device
	if err := d.Set(ThemeIgnore, 3, 3); err != nil {
		t.Fatalf("theme ignore must be a no-op, got %v", err)
	}
}

func TestNilDeviceSafe(t *testing.T) {
	var d *Device
	if err := d.Set(ThemeRainbow, 3, 3); err != nil {
		t.Fatalf("nil device Set: %v", err)
	}
	if err := d.Off(); err != nil {
		t.Fatalf("nil device Off: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("nil device Close: %v", err)
	}
}
