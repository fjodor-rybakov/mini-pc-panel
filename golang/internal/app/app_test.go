package app

import (
	"testing"

	"github.com/fjodor-rybakov/mini-pc-panel/golang/internal/config"
)

func TestDeviceRectLandscapeIsIdentity(t *testing.T) {
	r := config.Rect{X: 3, Y: 4, Width: 5, Height: 6}
	if got := DeviceRect(r, false); got != r {
		t.Fatalf("landscape: got %+v, want identity %+v", got, r)
	}
}

// Portrait: the logical layout is 170x320 but the wire buffer is ALWAYS
// 320x170 (spec §3), so widget rects are pre-rotated into device space:
// X' = Y, Y' = 170 - (X + Width), W' = Height, H' = Width.
func TestDeviceRectPortrait(t *testing.T) {
	r := config.Rect{X: 10, Y: 20, Width: 100, Height: 30}
	want := config.Rect{X: 20, Y: 60, Width: 30, Height: 100}
	if got := DeviceRect(r, true); got != want {
		t.Fatalf("portrait: got %+v, want %+v", got, want)
	}
	// The full portrait screen must map exactly onto the full 320x170 buffer.
	full := config.Rect{X: 0, Y: 0, Width: 170, Height: 320}
	wantFull := config.Rect{X: 0, Y: 0, Width: 320, Height: 170}
	if got := DeviceRect(full, true); got != wantFull {
		t.Fatalf("portrait full screen: got %+v, want %+v", got, wantFull)
	}
}
