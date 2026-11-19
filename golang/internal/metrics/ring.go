package metrics

// ring is a fixed-capacity numeric history; sensors size it by their
// configured max_points (original default 300) and pre-fill it with zeros.
type ring struct {
	size int
	buf  []float64
	next int
	n    int
}

func newRing(size int) *ring {
	if size < 1 {
		size = 1
	}
	return &ring{size: size, buf: make([]float64, size)}
}

func (r *ring) push(v float64) {
	r.buf[r.next] = v
	r.next = (r.next + 1) % r.size
	if r.n < r.size {
		r.n++
	}
}

// values returns the history oldest -> newest.
func (r *ring) values() []float64 {
	out := make([]float64, r.n)
	start := (r.next - r.n + r.size) % r.size
	for i := range out {
		out[i] = r.buf[(start+i)%r.size]
	}
	return out
}
