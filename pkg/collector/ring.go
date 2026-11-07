package collector

// ring keeps the last cap values pushed. It grows a value at a time up to cap,
// so a sandbox that has lived for a minute holds six points, not a full hour's
// worth of space. Not safe for concurrent use.
type ring[T any] struct {
	buf  []T
	head int // where the oldest value is once buf is full; 0 before that
	cap  int
}

func newRing[T any](capacity int) *ring[T] {
	return &ring[T]{cap: capacity}
}

// push adds v, dropping the oldest value once cap are held.
func (r *ring[T]) push(v T) {
	if len(r.buf) < r.cap {
		r.buf = append(r.buf, v)
		return
	}
	r.buf[r.head] = v
	r.head = (r.head + 1) % r.cap
}

func (r *ring[T]) len() int { return len(r.buf) }

// last is the newest value.
func (r *ring[T]) last() (v T, ok bool) {
	if len(r.buf) == 0 {
		return v, false
	}
	if len(r.buf) < r.cap {
		return r.buf[len(r.buf)-1], true
	}
	return r.buf[(r.head+r.cap-1)%r.cap], true
}

// appendTo appends the values, oldest first, to dst.
func (r *ring[T]) appendTo(dst []T) []T {
	dst = append(dst, r.buf[r.head:]...)
	return append(dst, r.buf[:r.head]...)
}
