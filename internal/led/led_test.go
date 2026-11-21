package led

import (
	"bytes"
	"errors"
	"testing"
	"time"
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

// fakeSink records every Write payload so the test can see the frame was
// delivered one byte at a time (port_byte_write in led_device.js), not as
// a single burst the HT32 LED parser drops.
type fakeSink struct {
	writes  [][]byte
	drained int
	writeAt []time.Time
	err     error
}

func (f *fakeSink) Write(p []byte) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.writes = append(f.writes, append([]byte(nil), p...))
	f.writeAt = append(f.writeAt, time.Now())
	return len(p), nil
}

func (f *fakeSink) Drain() error { f.drained++; return nil }

func TestWriteFramePacesOneBytePerWrite(t *testing.T) {
	var sleeps []time.Duration
	sink := &fakeSink{}
	frame := OffPacket()
	if err := writeFrame(sink, frame, func(d time.Duration) { sleeps = append(sleeps, d) }); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}
	if len(sink.writes) != len(frame) {
		t.Fatalf("got %d writes, want %d (one per byte)", len(sink.writes), len(frame))
	}
	var got []byte
	for _, w := range sink.writes {
		if len(w) != 1 {
			t.Fatalf("write of %d bytes, want 1", len(w))
		}
		got = append(got, w[0])
	}
	if !bytes.Equal(got, frame) {
		t.Errorf("bytes on the wire = % X, want % X", got, frame)
	}
	if len(sleeps) != len(frame)-1 {
		t.Fatalf("got %d pauses, want %d", len(sleeps), len(frame)-1)
	}
	for _, d := range sleeps {
		if d != byteDelay {
			t.Errorf("pause = %v, want %v", d, byteDelay)
		}
	}
	if sink.drained != 1 {
		t.Errorf("Drain called %d times, want 1", sink.drained)
	}
}

// Real wall-clock check: with time.Sleep the last byte must leave at least
// (len-1)*byteDelay after the first, i.e. the line idles between bytes like
// the original 5 ms setTimeout loop does.
func TestWriteFrameRealTiming(t *testing.T) {
	sink := &fakeSink{}
	if err := writeFrame(sink, OffPacket(), time.Sleep); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}
	want := time.Duration(len(OffPacket())-1) * byteDelay
	if el := sink.writeAt[len(sink.writeAt)-1].Sub(sink.writeAt[0]); el < want {
		t.Errorf("last byte after %v, want >= %v", el, want)
	}
}

func TestWriteFramePropagatesErrors(t *testing.T) {
	sink := &fakeSink{err: errors.New("boom")}
	if err := writeFrame(sink, OffPacket(), func(time.Duration) {}); err == nil {
		t.Fatal("write error must propagate")
	} else if !errors.Is(err, sink.err) {
		t.Errorf("error %v does not wrap the port error", err)
	}
}
