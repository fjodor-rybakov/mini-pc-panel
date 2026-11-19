package display

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

// Transport abstracts the HID device so the queue/worker logic is testable
// cross-platform (the real go-hid transport only builds on Linux).
type Transport interface {
	Write(packet []byte) error
	Close() error
}

// job is one unit of work for the single writer goroutine. A full redraw is
// kept as ONE job so its 27 chunks stay contiguous on the wire - heartbeats
// posted while it is queued cannot interleave between chunks (matching the
// original worker-thread queue semantics).
type job struct {
	packets [][]byte
	label   string
}

// Display serialises every command to the panel through ONE worker goroutine
// with a mandatory pause between packets (spec §5.1): the HT32 firmware hangs
// when fed too fast.
type Display struct {
	tr    Transport
	delay time.Duration
	// redrawDelay mirrors config.refresh in the original main.js with_delay:
	// the job after a 'redraw' waits this long before its first packet.
	redrawDelay time.Duration
	queue       chan *job
	quit        chan struct{}
	wg          sync.WaitGroup

	mu      sync.Mutex
	lastJob time.Time // when the most recent job was queued (heartbeat rule)
}

// New wraps a Transport. delay is the inter-packet pause (spec: 10 ms);
// redrawDelay is the post-redraw pause from the original with_delay (0
// selects the original default of 1600 ms, config.refresh).
func New(tr Transport, delay, redrawDelay time.Duration) *Display {
	if delay <= 0 {
		delay = 10 * time.Millisecond
	}
	if redrawDelay <= 0 {
		redrawDelay = 1600 * time.Millisecond
	}
	return &Display{
		tr:          tr,
		delay:       delay,
		redrawDelay: redrawDelay,
		queue:       make(chan *job, 256),
		quit:        make(chan struct{}),
	}
}

// Start launches the single writer goroutine.
func (d *Display) Start(ctx context.Context) {
	d.wg.Add(1)
	go d.worker(ctx)
}

func (d *Display) worker(ctx context.Context) {
	defer d.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return // graceful shutdown: pending jobs are dropped, descriptors closed by Close()
		case <-d.quit:
			return
		case j := <-d.queue:
			d.process(j)
			if j.label == "redraw" {
				// Original with_delay (main.js): the NEXT job waits
				// config.refresh ms after a full redraw.
				select {
				case <-time.After(d.redrawDelay):
				case <-d.quit:
					return
				}
			}
		}
	}
}

// process runs on the worker goroutine only.
func (d *Display) process(j *job) {
	for _, p := range j.packets {
		if err := d.tr.Write(p); err != nil {
			// The original lcd_thread.js logged and carried on; do the same.
			log.Printf("display: %s: write error: %v", j.label, err)
			return
		}
		time.Sleep(d.delay) // spec §5.1: pause between packets
	}
}

func (d *Display) post(ctx context.Context, j *job) error {
	// Decide "closed" deterministically: without this guard a buffered
	// queue slot and the closed quit channel are both ready and select
	// picks at random.
	select {
	case <-d.quit:
		return fmt.Errorf("display: closed")
	default:
	}
	select {
	case d.queue <- j:
		d.mu.Lock()
		d.lastJob = time.Now()
		d.mu.Unlock()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-d.quit:
		return fmt.Errorf("display: closed")
	}
}

// IdleFor reports how long ago the most recent job was queued. Before the
// first job it reports a very long idle period so the caller's original
// "queue idle longer than refresh ms -> heartbeat" rule fires immediately.
func (d *Display) IdleFor() time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lastJob.IsZero() {
		return 24 * time.Hour
	}
	return time.Since(d.lastJob)
}

// Pending reports how many jobs are queued but not yet fully written.
// The original heartbeat rule consults the queue length: a heartbeat is
// only meaningful while the panel has work pending.
func (d *Display) Pending() int { return len(d.queue) }

// SetOrientation queues a set_orientation command (spec §2.1).
func (d *Display) SetOrientation(ctx context.Context, orientation byte) error {
	return d.post(ctx, &job{packets: [][]byte{OrientationPacket(orientation)}, label: "set_orientation"})
}

// SetTime queues a set_time command (spec §2.2, subcmd 0xF3).
func (d *Display) SetTime(ctx context.Context, t time.Time) error {
	p := TimePacket(SubSetTime, byte(t.Hour()), byte(t.Minute()), byte(t.Second()))
	return d.post(ctx, &job{packets: [][]byte{p}, label: "set_time"})
}

// Heartbeat queues the 1s heartbeat command (spec §2.2/§5.2, subcmd 0xF2).
func (d *Display) Heartbeat(ctx context.Context, t time.Time) error {
	p := TimePacket(SubHeartbeat, byte(t.Hour()), byte(t.Minute()), byte(t.Second()))
	return d.post(ctx, &job{packets: [][]byte{p}, label: "heartbeat"})
}

// Redraw queues a full framebuffer transfer (27 chunks, spec §2.3).
func (d *Display) Redraw(ctx context.Context, frame []byte) error {
	if len(frame) != FrameBytes {
		return fmt.Errorf("display: redraw frame must be %d bytes, got %d", FrameBytes, len(frame))
	}
	return d.post(ctx, &job{packets: RedrawPackets(frame), label: "redraw"})
}

// Update queues one partial region update (spec §2.4). Callers pass pixels
// already encoded RGB565 big-endian, row-major for the rect.
func (d *Display) Update(ctx context.Context, r Rect, pixels []byte) error {
	if r.Width <= 0 || r.Height <= 0 || r.Width > 255 || r.Height > 255 {
		return fmt.Errorf("display: update rect %dx%d out of uint8 range", r.Width, r.Height)
	}
	if r.Width*r.Height > MaxUpdatePixels {
		return fmt.Errorf("display: update region %dx%d exceeds %d pixels per packet", r.Width, r.Height, MaxUpdatePixels)
	}
	if len(pixels) < r.Width*r.Height*2 {
		return fmt.Errorf("display: update needs %d pixel bytes, got %d", r.Width*r.Height*2, len(pixels))
	}
	p := UpdatePacket(r.X, r.Y, r.Width, r.Height, pixels)
	return d.post(ctx, &job{packets: [][]byte{p}, label: "update"})
}

// Close stops the worker and closes the underlying transport, releasing the
// USB descriptors (spec §5.3).
func (d *Display) Close() error {
	select {
	case <-d.quit:
	default:
		close(d.quit)
	}
	d.wg.Wait()
	return d.tr.Close()
}
