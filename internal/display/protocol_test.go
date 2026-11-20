package display

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func allZero(t *testing.T, b []byte, ctx string) {
	t.Helper()
	for i, v := range b {
		if v != 0 {
			t.Fatalf("%s: byte %d is 0x%02X, want 0x00", ctx, i, v)
		}
	}
}

func TestOrientationPacket(t *testing.T) {
	for _, o := range []byte{OrientationLandscape, OrientationPortrait} {
		p := OrientationPacket(o)
		if len(p) != PacketSize {
			t.Fatalf("packet len %d, want %d", len(p), PacketSize)
		}
		want := []byte{0x55, 0xA1, 0xF1, o}
		if !bytes.Equal(p[:4], want) {
			t.Fatalf("header % X, want % X", p[:4], want)
		}
		allZero(t, p[4:], "orientation tail+payload")
	}
}

func TestTimePacket(t *testing.T) {
	hb := TimePacket(SubHeartbeat, 13, 45, 7)
	if !bytes.Equal(hb[:6], []byte{0x55, 0xA1, 0xF2, 13, 45, 7}) {
		t.Fatalf("heartbeat header % X", hb[:6])
	}
	allZero(t, hb[6:], "heartbeat tail")
	if len(hb) != PacketSize {
		t.Fatalf("heartbeat len %d", len(hb))
	}
	st := TimePacket(SubSetTime, 1, 2, 3)
	if !bytes.Equal(st[:6], []byte{0x55, 0xA1, 0xF3, 1, 2, 3}) {
		t.Fatalf("set_time header % X", st[:6])
	}
}

func TestUpdatePacket(t *testing.T) {
	pixels := make([]byte, 3*2*2)
	for i := range pixels {
		pixels[i] = byte(i + 1)
	}
	p := UpdatePacket(300, 169, 3, 2, pixels)
	if len(p) != PacketSize {
		t.Fatalf("len %d", len(p))
	}
	if p[0] != 0x55 || p[1] != CmdUpdate {
		t.Fatalf("signature/cmd %02X %02X", p[0], p[1])
	}
	if got := binary.LittleEndian.Uint16(p[2:4]); got != 300 {
		t.Fatalf("x LE = %d, want 300", got)
	}
	if got := binary.LittleEndian.Uint16(p[4:6]); got != 169 {
		t.Fatalf("y LE = %d, want 169", got)
	}
	if p[6] != 3 || p[7] != 2 {
		t.Fatalf("w/h = %d/%d", p[6], p[7])
	}
	if !bytes.Equal(p[HeaderSize:HeaderSize+len(pixels)], pixels) {
		t.Fatal("pixels not at payload offset 8")
	}
	allZero(t, p[HeaderSize+len(pixels):], "update padding")
}

func TestRedrawPacketsSpecLayout(t *testing.T) {
	frame := make([]byte, FrameBytes)
	for i := range frame {
		frame[i] = byte(i * 7)
	}
	// explicit: the package default is RedrawOriginalLayout (hardware-
	// proven), so spec-layout assertions must request that layout by name.
	pk := RedrawPacketsLayout(frame, RedrawSpecLayout)
	if len(pk) != ChunkCount {
		t.Fatalf("chunk count %d, want %d", len(pk), ChunkCount)
	}
	for i, p := range pk {
		seq := byte(i + 1)
		if len(p) != PacketSize {
			t.Fatalf("chunk %d len %d", i, len(p))
		}
		if p[0] != Signature || p[1] != CmdRedraw || p[3] != seq {
			t.Fatalf("chunk %d header %02X %02X seq %02X", i, p[0], p[1], p[3])
		}
		var sub byte
		switch i {
		case 0:
			sub = RedrawStart
		case ChunkCount - 1:
			sub = RedrawEnd
		default:
			sub = RedrawContinue
		}
		if p[2] != sub {
			t.Fatalf("chunk %d subcmd %02X, want %02X", i, p[2], sub)
		}
		length := PayloadSize
		if i == ChunkCount-1 {
			length = LastChunkLen
		}
		if got := binary.LittleEndian.Uint16(p[6:8]); got != uint16(length) {
			t.Fatalf("chunk %d len field %04X, want %04X", i, got, uint16(length))
		}
		// spec §2.3: offset uint16 LE, wraps to 0 at seq 0x11 (65536 truncates)
		if got := binary.LittleEndian.Uint16(p[4:6]); got != uint16(i*PayloadSize) {
			t.Fatalf("chunk %d (seq %02X) offset %04X, want %04X", i, seq, got, uint16(i*PayloadSize))
		}
		if seq == 0x11 && binary.LittleEndian.Uint16(p[4:6]) != 0 {
			t.Fatal("offset must wrap to 0 at seq 0x11")
		}
		if !bytes.Equal(p[8:8+length], frame[i*PayloadSize:i*PayloadSize+length]) {
			t.Fatalf("chunk %d payload mismatch", i)
		}
		allZero(t, p[8+length:], "chunk padding")
	}
}

func TestRedrawOriginalLayoutQuirk(t *testing.T) {
	// Reproduces lcd_device.js lines 103-105: offset/len written BIG-endian
	// at 5..6 / 7..8 while the payload copy overlaps p[8], so the length low
	// byte is overwritten by the first payload byte.
	frame := bytes.Repeat([]byte{0xAB}, FrameBytes)
	pk := RedrawPacketsLayout(frame, RedrawOriginalLayout)
	p := pk[0]
	// pin the package default: RedrawPackets must keep producing the
	// original Node layout, the one every shipping panel is driven with.
	if DefaultRedrawLayout != RedrawOriginalLayout {
		t.Fatal("DefaultRedrawLayout must be RedrawOriginalLayout (hardware-proven)")
	}
	if def := RedrawPackets(frame); !bytes.Equal(def[0], p) {
		t.Fatalf("RedrawPackets() deviates from the default layout: % X vs % X", def[0][:9], p[:9])
	}
	if p[5] != 0x00 || p[6] != 0x00 {
		t.Fatalf("first chunk offset %02X %02X, want BE(0)", p[5], p[6])
	}
	if p[7] != 0x10 {
		t.Fatalf("length high byte at p[7] = %02X, want 0x10 (BE 0x1000)", p[7])
	}
	if p[8] != 0xAB {
		t.Fatalf("p[8] = %02X, want 0xAB: payload must overwrite the length's low byte", p[8])
	}
	mid := pk[5] // offset 5*4096 = 20480 = 0x5000 big-endian
	if mid[5] != 0x50 || mid[6] != 0x00 {
		t.Fatalf("chunk 5 offset %02X %02X, want BE(0x5000)", mid[5], mid[6])
	}
	last := pk[ChunkCount-1]
	if last[7] != 0x09 || last[8] != 0xAB { // BE 0x0900, low byte overwritten
		t.Fatalf("last chunk len %02X / overlap %02X, want 09/AB", last[7], last[8])
	}
}

func TestSplitRect(t *testing.T) {
	r := Rect{X: 5, Y: 7, Width: 300, Height: 160}
	subs := SplitRect(r)
	if len(subs) < 2 {
		t.Fatalf("300x160 (48000 px) must split, got %d rects", len(subs))
	}
	total := 0
	for _, s := range subs {
		if s.Width*s.Height > MaxUpdatePixels {
			t.Fatalf("sub-rect %dx%d exceeds %d px", s.Width, s.Height, MaxUpdatePixels)
		}
		if s.Width > 255 || s.Height > 255 {
			t.Fatalf("sub-rect %dx%d exceeds uint8", s.Width, s.Height)
		}
		if s.X < r.X || s.Y < r.Y || s.X+s.Width > r.X+r.Width || s.Y+s.Height > r.Y+r.Height {
			t.Fatalf("sub-rect %+v escapes parent %+v", s, r)
		}
		total += s.Width * s.Height
	}
	if total != r.Width*r.Height {
		t.Fatalf("coverage %d px, want %d (gaps or overlaps)", total, r.Width*r.Height)
	}

	if got := SplitRect(Rect{X: 1, Y: 2, Width: 45, Height: 45}); len(got) != 1 || got[0] == (Rect{}) {
		t.Fatalf("45x45 (2025 px) must stay one rect: %+v", got)
	}
	if got := SplitRect(Rect{Width: 0, Height: 10}); got != nil {
		t.Fatalf("degenerate rect must yield nil, got %+v", got)
	}
}

// Regression for demo.json's custom_bar (300x6): 1800 px fits MaxUpdatePixels
// but width 300 does NOT fit the uint8 header side, so the old early-return
// made Update reject every partial refresh of the bar. Every piece must be
// valid for one update packet, and cover the parent exactly.
func TestSplitRectWideBarFitsUint8Sides(t *testing.T) {
	r := Rect{X: 10, Y: 162, Width: 300, Height: 6}
	subs := SplitRect(r)
	if len(subs) < 2 {
		t.Fatalf("300x6 must split for the uint8 header, got %d rects", len(subs))
	}
	total := 0
	for _, s := range subs {
		if s.Width <= 0 || s.Height <= 0 || s.Width > 255 || s.Height > 255 {
			t.Fatalf("sub-rect %dx%d not valid for one update packet", s.Width, s.Height)
		}
		if s.Width*s.Height > MaxUpdatePixels {
			t.Fatalf("sub-rect %dx%d exceeds %d px", s.Width, s.Height, MaxUpdatePixels)
		}
		total += s.Width * s.Height
	}
	if total != r.Width*r.Height {
		t.Fatalf("coverage %d px, want %d", total, r.Width*r.Height)
	}
}

// Oversized pixel input must not spill past the 4096-byte payload, and a
// negative side (caller bug) must not slice the packet buffer backwards.
func TestUpdatePacketPixelCaps(t *testing.T) {
	p := UpdatePacket(0, 0, 10, 10, bytes.Repeat([]byte{0xCD}, PayloadSize))
	if !bytes.Equal(p[HeaderSize:HeaderSize+200], bytes.Repeat([]byte{0xCD}, 200)) {
		t.Fatal("10x10 pixels must occupy exactly the first 200 payload bytes")
	}
	allZero(t, p[HeaderSize+200:], "oversized pixel input must not spill")

	big := UpdatePacket(0, 0, 255, 255, bytes.Repeat([]byte{0x77}, 2*PayloadSize))
	if !bytes.Equal(big[HeaderSize:], bytes.Repeat([]byte{0x77}, PayloadSize)) {
		t.Fatal("255x255 (130050 px) must fill the payload, capped at 4096 bytes")
	}

	neg := UpdatePacket(0, 0, -4, 2, []byte{1, 2, 3})
	if len(neg) != PacketSize {
		t.Fatalf("len %d", len(neg))
	}
	allZero(t, neg[HeaderSize:], "negative width must produce an empty payload, not a panic")
}
