package collector

import "time"

// incarnation says which run of a helper a sample is of: its pid and its
// cgroup both change when it restarts.
type incarnation struct {
	pid    int
	cgroup string
}

// sample is what was read of one thing at one instant, kept to be the earlier
// end of the next interval.
type sample struct {
	at time.Time
	// ident is the incarnation that was read. When it changes the counters
	// restarted from zero, whatever they read.
	ident incarnation
	usage Usage
	net   *Net
}

// deriveRates turns two consecutive samples of the same thing into rates. It
// reports false when there is no interval to speak of (the clock stood still or
// went back).
//
// A counter that is lower than it was, or one whose incarnation changed, was
// reset: the VM's helper restarted and its cgroup began again at zero. What it
// reads now is then what it counted since, not a negative difference. That is
// what Prometheus's rate() takes it for too, and it keeps the first interval of
// a restarted VM a measure of that VM.
func deriveRates(prev, cur *sample) (Rates, bool) {
	secs := cur.at.Sub(prev.at).Seconds()
	if secs <= 0 {
		return Rates{}, false
	}
	restarted := prev.ident != cur.ident
	delta := func(p, c uint64) float64 {
		if restarted || c < p {
			return float64(c)
		}
		return float64(c - p)
	}
	r := Rates{Seconds: secs}

	if p, c := prev.usage.CPU, cur.usage.CPU; p != nil && c != nil {
		r.CPUPercent = delta(p.UsageUsec, c.UsageUsec) / usecPerSec / secs * 100
	}
	if p, c := prev.usage.IO, cur.usage.IO; p != nil && c != nil {
		r.IOReadBytesPerSec = delta(p.ReadBytes, c.ReadBytes) / secs
		r.IOWriteBytesPerSec = delta(p.WriteBytes, c.WriteBytes) / secs
	}
	if p, c := prev.usage.Pressure, cur.usage.Pressure; p != nil && c != nil {
		// Stall time can't exceed the interval; the two clocks disagree by a
		// little, so a stalled-the-whole-time reading may come out just over.
		stall := func(p, c *PSI, full bool) float64 {
			if p == nil || c == nil {
				return 0
			}
			d := delta(p.SomeUsec, c.SomeUsec)
			if full {
				if !p.HasFull || !c.HasFull {
					return 0
				}
				d = delta(p.FullUsec, c.FullUsec)
			}
			return min(d/usecPerSec/secs*100, 100)
		}
		r.CPUSomePercent = stall(p.CPU, c.CPU, false)
		r.MemSomePercent = stall(p.Memory, c.Memory, false)
		r.MemFullPercent = stall(p.Memory, c.Memory, true)
		r.IOSomePercent = stall(p.IO, c.IO, false)
		r.IOFullPercent = stall(p.IO, c.IO, true)
	}
	if p, c := prev.net, cur.net; p != nil && c != nil {
		r.NetRxBytesPerSec = delta(p.RxBytes, c.RxBytes) / secs
		r.NetTxBytesPerSec = delta(p.TxBytes, c.TxBytes) / secs
	}
	return r, true
}

const usecPerSec = 1e6

// point is the ring entry for the interval a sandbox's rates cover.
func point(at time.Time, usage *Usage, r Rates) Point {
	p := Point{
		At:         at,
		CPUPercent: float32(r.CPUPercent),
		IOReadBPS:  float32(r.IOReadBytesPerSec),
		IOWriteBPS: float32(r.IOWriteBytesPerSec),
		CPUSome:    float32(r.CPUSomePercent),
		MemSome:    float32(r.MemSomePercent),
		MemFull:    float32(r.MemFullPercent),
		IOSome:     float32(r.IOSomePercent),
		NetRxBPS:   float32(r.NetRxBytesPerSec),
		NetTxBPS:   float32(r.NetTxBytesPerSec),
	}
	if usage.Memory != nil {
		p.MemoryBytes = usage.Memory.Current
	}
	return p
}
