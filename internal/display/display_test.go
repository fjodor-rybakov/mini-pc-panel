package display

import (
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeTransport records every packet the single worker goroutine writes,
// letting us assert serialisation and contiguity without HID hardware.
type fakeTransport struct {
	mu       sync.Mutex
	written  [][]byte
	closed   int
	failFrom int // -1 = never fail
}

func newFakeTransport() *fakeTransport { return &fakeTransport{failFrom: -1} }

func (f *fakeTransport) Write(p []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failFrom >= 0 && len(f.written) >= f.failFrom {
		return errors.New("fake write failure")
	}
	f.written = append(f.written, append([]byte(nil), p...))
	return nil
}

func (f *fakeTransport) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	return nil
}

func (f *fakeTransport) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.written)
}

func (f *fakeTransport) at(i int) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.written[i]
}

func waitForWrites(t *testing.T, f *fakeTransport, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for f.count() < n {
		if time.Now().After(deadline) {
			t.Fatalf("timeout: got %d writes, want %d", f.count(), n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// Spec §5.1/§5.2: one writer goroutine; a redraw is ONE job, so its 27
// chunks stay contiguous and a heartbeat queued right after can never slip
// between chunks.
func TestWorkerKeepsRedrawContiguousBeforeHeartbeat(t *testing.T) {
	tr := newFakeTransport()
	d := New(tr, time.Millisecond, 20*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)

	if err := d.Redraw(ctx, make([]byte, FrameBytes)); err != nil {
		t.Fatal(err)
	}
	if err := d.Heartbeat(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	waitForWrites(t, tr, ChunkCount+1)
	d.Close()

	for i := 0; i < ChunkCount; i++ {
		p := tr.at(i)
		if p[1] != CmdRedraw {
			t.Fatalf("write %d cmd %02X, want redraw", i, p[1])
		}
		if p[3] != byte(i+1) {
			t.Fatalf("write %d seq %02X out of order", i, p[3])
		}
	}
	if hb := tr.at(ChunkCount); hb[1] != CmdConfig || hb[2] != SubHeartbeat {
		t.Fatalf("heartbeat slipped to position %d: %02X %02X", ChunkCount, hb[1], hb[2])
	}
	if tr.closed != 1 {
		t.Fatalf("transport closed %d times by Close(), want 1", tr.closed)
	}
}

// Original with_delay (main.js): the job after a full redraw waits
// config.refresh ms (here 200 ms). IdleFor()/Pending() feed the original
// conditional heartbeat rule, so they are asserted here too.
func TestRedrawDelayPausesNextJob(t *testing.T) {
	tr := newFakeTransport()
	d := New(tr, time.Millisecond, 200*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)

	if err := d.Redraw(ctx, make([]byte, FrameBytes)); err != nil {
		t.Fatal(err)
	}
	if err := d.Heartbeat(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if d.Pending() < 1 {
		t.Fatal("Pending() must report the queued jobs")
	}
	if d.IdleFor() > time.Minute {
		t.Fatal("IdleFor() must measure time since the last queued job")
	}

	start := time.Now()
	waitForWrites(t, tr, ChunkCount+1)
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Fatalf("heartbeat came after %v; the post-redraw refresh pause was skipped", elapsed)
	}
	if hb := tr.at(ChunkCount); hb[1] != CmdConfig || hb[2] != SubHeartbeat {
		t.Fatalf("write after redraw block is not the heartbeat: %02X %02X", hb[1], hb[2])
	}
	d.Close()
}

func TestDisplayInputValidation(t *testing.T) {
	tr := newFakeTransport()
	d := New(tr, time.Millisecond, 20*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)

	if err := d.Redraw(ctx, make([]byte, 10)); err == nil {
		t.Fatal("short frame must be rejected")
	}
	if err := d.Update(ctx, Rect{X: 0, Y: 0, Width: 100, Height: 100}, make([]byte, 20000)); err == nil {
		t.Fatal("100x100 exceeds one update packet")
	}
	if err := d.Update(ctx, Rect{X: 0, Y: 0, Width: 300, Height: 2}, make([]byte, 1200)); err == nil {
		t.Fatal("width > 255 must be rejected")
	}
	if err := d.Update(ctx, Rect{X: 0, Y: 0, Width: 10, Height: 10}, make([]byte, 8)); err == nil {
		t.Fatal("too few pixel bytes must be rejected")
	}
	if n := tr.count(); n != 0 {
		t.Fatalf("rejected commands still wrote %d packets", n)
	}
	// Frame bounds: coords go on the wire as uint16 LE, so a negative
	// origin would hit the panel as 65535 and an oversized rect desyncs
	// it from the caller's diff baseline.
	if err := d.Update(ctx, Rect{X: 310, Y: 0, Width: 20, Height: 10}, make([]byte, 400)); err == nil {
		t.Fatal("rect past the right frame edge must be rejected")
	}
	if err := d.Update(ctx, Rect{X: 10, Y: 165, Width: 10, Height: 10}, make([]byte, 200)); err == nil {
		t.Fatal("rect past the bottom frame edge must be rejected")
	}
	if err := d.Update(ctx, Rect{X: -5, Y: 0, Width: 10, Height: 10}, make([]byte, 200)); err == nil {
		t.Fatal("negative origin must be rejected, not sent as 65535")
	}
	if err := d.Update(ctx, Rect{X: 10, Y: 10, Width: 10, Height: 10}, make([]byte, 200)); err != nil {
		t.Fatalf("valid update rejected: %v", err)
	}
	waitForWrites(t, tr, 1)
	d.Close()
}

// End-to-end for demo.json's custom_bar (300x6 at y=162): every piece
// SplitRect produces must pass Update's validation and reach the wire in
// order — the bug was Update rejecting the unsplit 300-wide rect, so the
// bar never refreshed partially.
func TestWideBarPiecesReachWire(t *testing.T) {
	tr := newFakeTransport()
	d := New(tr, time.Millisecond, 20*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)

	r := Rect{X: 10, Y: 162, Width: 300, Height: 6}
	subs := SplitRect(r)
	for _, s := range subs {
		if err := d.Update(ctx, s, make([]byte, s.Width*s.Height*2)); err != nil {
			t.Fatalf("piece %dx%d rejected: %v", s.Width, s.Height, err)
		}
	}
	waitForWrites(t, tr, len(subs))
	for i, s := range subs {
		p := tr.at(i)
		if p[1] != CmdUpdate {
			t.Fatalf("write %d cmd %02X, want update", i, p[1])
		}
		if x := int(binary.LittleEndian.Uint16(p[2:4])); x != s.X {
			t.Fatalf("write %d x=%d, want %d (pieces out of order)", i, x, s.X)
		}
	}
	d.Close()
}

// Transport errors are logged, never fatal (original lcd_thread.js kept
// running after write failures).
func TestWriteFailureDoesNotKillWorker(t *testing.T) {
	tr := &fakeTransport{failFrom: 0}
	d := New(tr, time.Millisecond, 20*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)

	if err := d.SetOrientation(ctx, OrientationLandscape); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // worker processed (and logged) the failure

	// The abandoned job diverged the panel from the caller's baseline; the
	// poll loop watches this counter to latch a resync full redraw.
	if n := d.WriteFailures(); n != 1 {
		t.Fatalf("WriteFailures() = %d after one abandoned job, want 1", n)
	}

	tr.mu.Lock()
	tr.failFrom = -1
	tr.mu.Unlock()
	if err := d.SetTime(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	waitForWrites(t, tr, 1)
	d.Close()
}

// Regression for the "panel refreshes in shifted/torn bands" symptom: the
// SplitRect pieces of ONE widget go as one job, so a heartbeat queued while
// the worker is mid-job can never slip between the widget's tiles.
func TestUpdatesKeepsWidgetPiecesContiguous(t *testing.T) {
	tr := newFakeTransport()
	d := New(tr, 10*time.Millisecond, 20*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)

	subs := SplitRect(Rect{X: 10, Y: 162, Width: 300, Height: 6})
	if len(subs) < 2 {
		t.Fatalf("SplitRect produced %d pieces; need a multi-piece region", len(subs))
	}
	if err := d.Updates(ctx, subs, func(r Rect) []byte {
		return make([]byte, r.Width*r.Height*2)
	}); err != nil {
		t.Fatal(err)
	}
	// Queue the heartbeat while the worker is still writing the first
	// piece: with separate Update() calls it used to interleave between
	// tiles; as one job it must stay behind the whole block.
	if err := d.Heartbeat(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	waitForWrites(t, tr, len(subs)+1)
	d.Close()

	for i, s := range subs {
		p := tr.at(i)
		if p[1] != CmdUpdate {
			t.Fatalf("write %d cmd %02X, want update (pieces interleaved)", i, p[1])
		}
		if x := int(binary.LittleEndian.Uint16(p[2:4])); x != s.X {
			t.Fatalf("write %d x=%d, want %d (pieces out of order)", i, x, s.X)
		}
	}
	if hb := tr.at(len(subs)); hb[1] != CmdConfig || hb[2] != SubHeartbeat {
		t.Fatalf("heartbeat did not follow the update block: %02X %02X", hb[1], hb[2])
	}
}

// Updates rejects an invalid piece without posting the good pieces too - a
// half-accepted batch would diverge the panel from the diff baseline.
func TestUpdatesValidationRejectsWithoutPosting(t *testing.T) {
	tr := newFakeTransport()
	d := New(tr, time.Millisecond, 20*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)

	if err := d.Updates(ctx, nil, func(Rect) []byte { return nil }); err != nil {
		t.Fatalf("empty rects list should be a no-op, got %v", err)
	}
	good := Rect{X: 0, Y: 0, Width: 10, Height: 10}
	bad := Rect{X: 310, Y: 0, Width: 20, Height: 10} // past the right frame edge
	if err := d.Updates(ctx, []Rect{good, bad}, func(r Rect) []byte {
		return make([]byte, r.Width*r.Height*2)
	}); err == nil {
		t.Fatal("bad piece must reject the whole batch")
	}
	if d.Pending() != 0 {
		t.Fatal("rejected batch must not enter the queue")
	}
	if err := d.Updates(ctx, []Rect{good}, func(r Rect) []byte {
		return make([]byte, r.Width*r.Height*2)
	}); err != nil {
		t.Fatalf("valid batch rejected: %v", err)
	}
	waitForWrites(t, tr, 1)
	d.Close()
}

// A resync full redraw must not queue behind the stale partials it
// supersedes: DropPending drains queued update jobs but keeps commands the
// redraw does not supersede (orientation, heartbeat) in order.
func TestDropPendingDropsOnlyQueuedUpdates(t *testing.T) {
	tr := newFakeTransport()
	d := New(tr, time.Millisecond, 20*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// No Start yet: the queue stays untouched while we drain it.

	if err := d.Update(ctx, Rect{X: 0, Y: 0, Width: 10, Height: 10}, make([]byte, 200)); err != nil {
		t.Fatal(err)
	}
	if err := d.Update(ctx, Rect{X: 20, Y: 0, Width: 10, Height: 10}, make([]byte, 200)); err != nil {
		t.Fatal(err)
	}
	if err := d.SetOrientation(ctx, OrientationPortrait); err != nil {
		t.Fatal(err)
	}
	if err := d.Heartbeat(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if n := d.DropPending(); n != 2 {
		t.Fatalf("DropPending() = %d, want 2", n)
	}
	if p := d.Pending(); p != 2 {
		t.Fatalf("Pending() = %d after drain, want 2 (orientation + heartbeat kept)", p)
	}

	d.Start(ctx)
	waitForWrites(t, tr, 2)
	d.Close()
	if p := tr.at(0); p[1] != CmdConfig || p[2] != SubOrientation {
		t.Fatalf("first surviving job is not the orientation command: %02X %02X", p[1], p[2])
	}
	if p := tr.at(1); p[1] != CmdConfig || p[2] != SubHeartbeat {
		t.Fatalf("second surviving job is not the heartbeat: %02X %02X", p[1], p[2])
	}
}

func TestPostAfterCloseFails(t *testing.T) {
	tr := newFakeTransport()
	d := New(tr, time.Millisecond, 20*time.Millisecond)
	ctx := context.Background()
	d.Start(ctx)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if err := d.Heartbeat(ctx, time.Now()); err == nil {
		t.Fatal("posting after Close must fail")
	}
	// Double Close (Run's defer + main's explicit call, say): the HID
	// descriptor close must run exactly once.
	if err := d.Close(); err != nil {
		t.Fatalf("second Close must be a no-op, got %v", err)
	}
	if tr.closed != 1 {
		t.Fatalf("transport closed %d times, want 1", tr.closed)
	}
}
