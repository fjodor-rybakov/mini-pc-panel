// Package display implements the HT32 panel protocol over USB HID
// (doc/revrite.md §2): every command is a fixed 4104-byte packet.
package display

import (
	"encoding/binary"
	"fmt"
)

// Protocol constants (spec §2).
const (
	Signature = 0x55

	CmdConfig byte = 0xA1 // orientation / set time / heartbeat
	CmdUpdate byte = 0xA2 // partial region update
	CmdRedraw byte = 0xA3 // full framebuffer transfer

	SubOrientation byte = 0xF1
	SubHeartbeat   byte = 0xF2
	SubSetTime     byte = 0xF3

	OrientationLandscape byte = 0x01 // 320x170
	OrientationPortrait  byte = 0x02 // 170x320

	RedrawStart    byte = 0xF0 // seq 0x01
	RedrawContinue byte = 0xF1 // seq 0x02..0x1A
	RedrawEnd      byte = 0xF2 // seq 0x1B

	HeaderSize   = 8
	PayloadSize  = 4096
	PacketSize   = HeaderSize + PayloadSize // 4104 bytes on the wire (excluding report ID)
	ChunkCount   = 27                       // 27 * 4096 = 110592 >= 108800
	LastChunkLen = 2304                     // final chunk pixel bytes: 108800 - 26*4096

	FrameWidth  = 320
	FrameHeight = 170
	FrameBytes  = FrameWidth * FrameHeight * 2 // RGB565 big-endian = 108800

	// MaxUpdatePixels bounds one update packet: width*height*2 <= 4096.
	MaxUpdatePixels = PayloadSize / 2
)

// OrientationPacket builds the set_orientation config packet.
func OrientationPacket(orientation byte) []byte {
	p := make([]byte, PacketSize)
	p[0] = Signature
	p[1] = CmdConfig
	p[2] = SubOrientation
	p[3] = orientation
	return p
}

// TimePacket builds a set_time (SubSetTime) or heartbeat (SubHeartbeat) packet.
// subcmd 0xF2 doubles as the 1s heartbeat per spec §2.2 and §5.2; it carries
// the current clock like the original heartbeat() did.
func TimePacket(subcmd byte, hour, minute, second byte) []byte {
	p := make([]byte, PacketSize)
	p[0] = Signature
	p[1] = CmdConfig
	p[2] = subcmd
	p[3] = hour
	p[4] = minute
	p[5] = second
	return p
}

// UpdatePacket builds an update (0xA2) packet. Coordinates are uint16 LE,
// width/height uint8, payload row-major RGB565 big-endian (spec §2.4).
func UpdatePacket(x, y, width, height int, pixels []byte) []byte {
	p := make([]byte, PacketSize)
	p[0] = Signature
	p[1] = CmdUpdate
	binary.LittleEndian.PutUint16(p[2:4], uint16(x))
	binary.LittleEndian.PutUint16(p[4:6], uint16(y))
	p[6] = byte(width)
	p[7] = byte(height)
	// Copy only the w*h*2 region: oversized input must not spill past the
	// payload (and negative width/height must not slice backwards).
	n := width * height * 2
	if n < 0 {
		n = 0 // negative side (caller error): send an empty region, no panic
	}
	if n > PayloadSize {
		n = PayloadSize
	}
	n = min(n, len(pixels)) // short input stays zero-padded, like redraw chunks
	copy(p[HeaderSize:HeaderSize+n], pixels)
	return p
}

// RedrawLayout selects the byte layout of redraw chunk headers.
type RedrawLayout int

const (
	// RedrawSpecLayout follows doc/revrite.md §2.3:
	// [0x55, 0xA3, subcmd, seq, offset_lo, offset_hi, len_lo, len_hi],
	// offset/length uint16 little-endian, payload starts at byte 8.
	RedrawSpecLayout RedrawLayout = iota

	// RedrawOriginalLayout reproduces the upstream Node implementation
	// (s1panel/lcd_device.js redraw, lines 103-105): seq at byte 3, byte 4
	// unused, offset uint16 BIG-endian at bytes 5..6, length uint16
	// BIG-endian at bytes 7..8, payload written starting at byte 8 after
	// the header - so the length's low byte at p[8] is overwritten by the
	// first payload byte, exactly the quirk of the original code (payload
	// copy happens after the header is set).
	//
	// Both layouts are observed working on real hardware: the firmware keys
	// the transfer on subcmd/seq and streams chunks sequentially, so the
	// offset/length fields are informational. The original Node layout is
	// the proven one - every shipping s1panel install drives panels with
	// it - so it is the default; switch DefaultRedrawLayout to
	// RedrawSpecLayout if you trust the doc/revrite.md §2.3 wording more.
	RedrawOriginalLayout
)

// DefaultRedrawLayout is what RedrawPackets uses: the original Node layout.
const DefaultRedrawLayout = RedrawOriginalLayout

// RedrawPackets splits a full 108800-byte RGB565 frame into the 27 chunks
// described by spec §2.3 using DefaultRedrawLayout.
func RedrawPackets(frame []byte) [][]byte {
	return RedrawPacketsLayout(frame, DefaultRedrawLayout)
}

// RedrawPacketsLayout splits the frame according to the given layout.
func RedrawPacketsLayout(frame []byte, layout RedrawLayout) [][]byte {
	if len(frame) != FrameBytes {
		panic(fmt.Sprintf("display: frame must be %d bytes, got %d", FrameBytes, len(frame)))
	}
	packets := make([][]byte, ChunkCount)
	for i := 0; i < ChunkCount; i++ {
		seq := byte(i + 1)
		var subcmd byte
		switch {
		case i == 0:
			subcmd = RedrawStart
		case i == ChunkCount-1:
			subcmd = RedrawEnd
		default:
			subcmd = RedrawContinue
		}
		length := PayloadSize
		if i == ChunkCount-1 {
			length = LastChunkLen
		}
		offset := i * PayloadSize // wraps to 0 at seq 0x11 (uint16 truncation) per spec

		p := make([]byte, PacketSize)
		p[0] = Signature
		p[1] = CmdRedraw
		p[2] = subcmd
		p[3] = seq
		var dataAt int
		switch layout {
		case RedrawOriginalLayout:
			// p[4] stays 0 (unused); original JS wrote offset/len big-endian
			// one byte further along, with the length low byte landing on the
			// first payload byte (then overwritten by the payload itself).
			binary.BigEndian.PutUint16(p[5:7], uint16(offset))
			binary.BigEndian.PutUint16(p[7:9], uint16(length))
			dataAt = 8 // payload overlaps p[8], matching the original's ordering
		default: // RedrawSpecLayout
			binary.LittleEndian.PutUint16(p[4:6], uint16(offset))
			binary.LittleEndian.PutUint16(p[6:8], uint16(length))
			dataAt = 8
		}
		copy(p[dataAt:dataAt+length], frame[i*PayloadSize:i*PayloadSize+length])
		packets[i] = p
	}
	return packets
}

// Rect is a device-space (320x170 buffer) region for the update command.
type Rect struct {
	X      int
	Y      int
	Width  int
	Height int
}

// SplitRect chops a device rect into grid pieces small enough for one update
// packet each (width*height*2 <= 4096 bytes), mirroring calc_update_region of
// the original main.js: sides are bounded by ~sqrt(MaxUpdatePixels) = 45 px.
func SplitRect(r Rect) []Rect {
	if r.Width <= 0 || r.Height <= 0 {
		return nil
	}
	// Both bounds matter: the packet header carries width/height as uint8,
	// so a 300x6 bar (1800 px <= MaxUpdatePixels) still must be split —
	// Update would otherwise reject every partial refresh of it.
	if r.Width*r.Height <= MaxUpdatePixels && r.Width <= 255 && r.Height <= 255 {
		return []Rect{r}
	}
	const side = 45
	cols := ceilDiv(r.Width, side)
	rows := ceilDiv(r.Height, side)
	cw := ceilDiv(r.Width, cols)
	ch := ceilDiv(r.Height, rows)
	var out []Rect
	for y := 0; y < r.Height; y += ch {
		hh := min(ch, r.Height-y)
		for x := 0; x < r.Width; x += cw {
			ww := min(cw, r.Width-x)
			out = append(out, Rect{X: r.X + x, Y: r.Y + y, Width: ww, Height: hh})
		}
	}
	return out
}

func ceilDiv(a, b int) int { return (a + b - 1) / b }
