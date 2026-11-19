package display

import "testing"

// Vendor screen interface, simplified to the discriminator fields: usage
// page 0xFF00, one Output report of 4104 bytes (Report Size 8, Count 0x1008).
var screenDesc = []byte{
	0x06, 0x00, 0xFF, 0x09, 0x01, 0xA1, 0x01,
	0x09, 0x02, 0x15, 0x00, 0x26, 0xFF, 0x00,
	0x75, 0x08, 0x96, 0x08, 0x10, // Report Size 8, Report Count 4104
	0x91, 0x82, // Output (Main, tag 9)
	0xC0,
}

// Boot-keyboard LED interface: 8-bit Output report — the wrong node to open.
var keyboardDesc = []byte{
	0x05, 0x08, 0x19, 0x01, 0x29, 0x08,
	0x75, 0x01, 0x95, 0x08, 0x91, 0x02,
	0x75, 0x08, 0x95, 0x01, 0x81, 0x01,
	0xC0,
}

func TestMaxOutputReportBits(t *testing.T) {
	if got := maxOutputReportBits(screenDesc); got != 8*4104 {
		t.Errorf("screen descriptor: got %d bits, want %d", got, 8*4104)
	}
	if got := maxOutputReportBits(keyboardDesc); got != 8 {
		t.Errorf("keyboard descriptor: got %d bits, want 8", got)
	}
	withLong := append([]byte{0xFE, 0x03, 0x01, 0x02, 0x03}, screenDesc...)
	if got := maxOutputReportBits(withLong); got != 8*4104 {
		t.Errorf("descriptor prefixed with a long item: got %d bits, want %d", got, 8*4104)
	}
}

func TestMatchProductUEvent(t *testing.T) {
	uevent := "HID_NAME=ACE MAGIC S1\nHID_BUS=0003\nMODALIAS=hid:b0000v04D9pFD01e0100\nPRODUCT=4d9/fd01/1102\nHID_ID=0003000004D90000FD01\n"
	vid, pid, ok := matchProductUEvent(uevent)
	if !ok || vid != 0x04D9 || pid != 0xFD01 {
		t.Errorf("S1 uevent: got vid=%04x pid=%04x ok=%v", vid, pid, ok)
	}
	vid, pid, ok = matchProductUEvent("PRODUCT=1a86/7523/0100\n")
	if !ok || vid != 0x1A86 || pid != 0x7523 {
		t.Errorf("CH340-style uevent: got vid=%04x pid=%04x ok=%v", vid, pid, ok)
	}
	if _, _, ok := matchProductUEvent("HID_NAME=no product line\n"); ok {
		t.Error("uevent without PRODUCT line must not match")
	}
	// Older kernels: no PRODUCT= line in HID uevents, MODALIAS= only.
	vid, pid, ok = matchProductUEvent("HID_NAME=ACE MAGIC S1\nHID_ID=0003000004D90000FD01\nMODALIAS=hid:b0000v04D9pFD01e0110\n")
	if !ok || vid != 0x04D9 || pid != 0xFD01 {
		t.Errorf("modalias-only uevent: got vid=%04x pid=%04x ok=%v", vid, pid, ok)
	}
	if _, _, ok := matchProductUEvent("MODALIAS=hid:b0003v12pABCD0e0001\nHID_NAME=Mouse\n"); ok {
		t.Error("malformed modalias must not match")
	}
	if vid, pid, ok = parseHIDModalias("hid:b0005v0A0Bp0C0De0F0E"); !ok || vid != 0x0A0B || pid != 0x0C0D {
		t.Errorf("parseHIDModalias: got vid=%04x pid=%04x ok=%v", vid, pid, ok)
	}
}

func TestLastElem(t *testing.T) {
	if got := lastElem("../../devices/pci0000:00/usb1/1-2/1-2:1.2/0003:04D9:FD01.0003"); got != "0003:04D9:FD01.0003" {
		t.Errorf("lastElem sysfs link: got %q", got)
	}
	if got := lastElem("bare"); got != "bare" {
		t.Errorf("lastElem bare: got %q", got)
	}
	if got := lastElem(""); got != "" {
		t.Errorf("lastElem empty: got %q", got)
	}
}
