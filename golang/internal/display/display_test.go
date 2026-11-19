package display

import (
	"context"
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
	if err := d.Update(ctx, Rect{X: 10, Y: 10, Width: 10, Height: 10}, make([]byte, 200)); err != nil {
		t.Fatalf("valid update rejected: %v", err)
	}
	waitForWrites(t, tr, 1)
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

	tr.mu.Lock()
	tr.failFrom = -1
	tr.mu.Unlock()
	if err := d.SetTime(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	waitForWrites(t, tr, 1)
	d.Close()
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
}
