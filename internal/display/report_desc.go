package display

import (
	"strconv"
	"strings"
)

// maxOutputReportBits returns the bit length of the largest Output report
// declared by a raw HID report descriptor (the bytes of
// /sys/class/hidraw/hidrawN/device/report_descriptor). The S1 screen
// interface declares one 4104-byte output report (32832 bits); a
// button/keyboard interface declares at most a few bits of LEDs.
//
// Short-item header layout: bTag bits 7..4, bType bits 3..2 (0 Main,
// 1 Global, 2 Local), bSize bits 1..0 (size 3 means four data bytes).
// Global Report Size = tag 7 (0x75/0x76), Report Count = tag 9 (0x95/0x96),
// Main Output = tag 9 (0x90..0x93). Long items (0xFE ...) are skipped.
func maxOutputReportBits(desc []byte) int {
	size, count, maxBits := 0, 0, 0
	for i := 0; i < len(desc); {
		head := desc[i]
		if head == 0xFE {
			if i+1 >= len(desc) {
				break
			}
			i += 2 + int(desc[i+1])
			continue
		}
		ln := int(head & 0x03)
		if ln == 3 {
			ln = 4
		}
		if i+1+ln > len(desc) {
			break
		}
		val := 0
		for b := 0; b < ln; b++ {
			val |= int(desc[i+1+b]) << (8 * b)
		}
		switch {
		case (head>>2)&0x03 == 1 && head>>4 == 7: // Global — Report Size
			size = val
		case (head>>2)&0x03 == 1 && head>>4 == 9: // Global — Report Count
			count = val
		case (head>>2)&0x03 == 0 && head>>4 == 9: // Main — Output
			if bits := size * count; bits > maxBits {
				maxBits = bits
			}
		}
		i += 1 + ln
	}
	return maxBits
}

// matchProductUEvent parses the "PRODUCT=<vid>/<pid>/<rev>" line of a HID
// device uevent (kernel writes plain lowercase hex without leading zeros,
// e.g. "PRODUCT=4d9/fd01/1102").
func matchProductUEvent(uevent string) (vid, pid uint16, ok bool) {
	for _, line := range strings.Split(uevent, "\n") {
		v, found := strings.CutPrefix(strings.TrimSpace(line), "PRODUCT=")
		if !found {
			continue
		}
		parts := strings.Split(v, "/")
		if len(parts) < 2 {
			continue
		}
		u, err1 := strconv.ParseUint(parts[0], 16, 16)
		p, err2 := strconv.ParseUint(parts[1], 16, 16)
		if err1 != nil || err2 != nil {
			continue
		}
		return uint16(u), uint16(p), true
	}
	return 0, 0, false
}
