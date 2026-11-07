package collector

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"syscall"
)

// The cgroup v2 accounting files, and how they are read. Every parser takes the
// file's bytes and returns what it holds; reading, and what to do when a file
// isn't there, is usageReader's.

// errGone says the cgroup was removed between listing it and reading it: its
// last process exited. Expected, and not a failure of the sample.
var errGone = errors.New("cgroup is gone")

// nextLine splits b at its first newline.
func nextLine(b []byte) (line, rest []byte) {
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		return b[:i], b[i+1:]
	}
	return b, nil
}

// atou parses an unsigned decimal; false for anything else, including overflow.
func atou(b []byte) (uint64, bool) {
	if len(b) == 0 {
		return 0, false
	}
	var v uint64
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
		d := uint64(c - '0')
		if v > (1<<64-1-d)/10 {
			return 0, false
		}
		v = v*10 + d
	}
	return v, true
}

// keyValue splits a "key value" line, the shape of cpu.stat, memory.stat and
// memory.events.
func keyValue(line []byte) (key, val []byte, ok bool) {
	i := bytes.IndexByte(line, ' ')
	if i <= 0 {
		return nil, nil, false
	}
	return line[:i], bytes.TrimSpace(line[i+1:]), true
}

// eachKeyValue calls fn for every line of a "key value" file and fails on a
// line that isn't one or whose value isn't a number: a kernel that changed the
// format should be noticed, not read as zeros. Blank lines are skipped.
func eachKeyValue(b []byte, fn func(key []byte, v uint64)) error {
	for len(b) > 0 {
		var line []byte
		line, b = nextLine(b)
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		key, val, ok := keyValue(line)
		if !ok {
			return fmt.Errorf("line %q is not a key and a value", line)
		}
		v, ok := atou(val)
		if !ok {
			return fmt.Errorf("line %q: %q is not a number", line, val)
		}
		fn(key, v)
	}
	return nil
}

// parseUint parses a file that holds one number (memory.current, memory.peak).
func parseUint(b []byte) (uint64, error) {
	s := bytes.TrimSpace(b)
	v, ok := atou(s)
	if !ok {
		return 0, fmt.Errorf("%q is not a number", s)
	}
	return v, nil
}

// parseCPUStat reads cpu.stat. Its other keys (throttling, core scheduling)
// vary with the kernel and aren't used.
func parseCPUStat(b []byte) (CPU, error) {
	var c CPU
	var seen bool
	err := eachKeyValue(b, func(key []byte, v uint64) {
		switch string(key) {
		case "usage_usec":
			c.UsageUsec, seen = v, true
		case "user_usec":
			c.UserUsec = v
		case "system_usec":
			c.SystemUsec = v
		}
	})
	if err == nil && !seen {
		err = errors.New("no usage_usec")
	}
	return c, err
}

// parseMemoryStat reads the anonymous and file-backed bytes out of memory.stat.
func parseMemoryStat(b []byte) (anon, file uint64, err error) {
	var seenAnon, seenFile bool
	err = eachKeyValue(b, func(key []byte, v uint64) {
		switch string(key) {
		case "anon":
			anon, seenAnon = v, true
		case "file":
			file, seenFile = v, true
		}
	})
	if err == nil && !(seenAnon && seenFile) {
		err = errors.New("no anon and file")
	}
	return anon, file, err
}

// parseMemEvents reads memory.events. Keys a kernel doesn't have (oom_group_kill
// before 5.17) stay zero.
func parseMemEvents(b []byte) (MemEvents, error) {
	var e MemEvents
	err := eachKeyValue(b, func(key []byte, v uint64) {
		switch string(key) {
		case "low":
			e.Low = v
		case "high":
			e.High = v
		case "max":
			e.Max = v
		case "oom":
			e.OOM = v
		case "oom_kill":
			e.OOMKill = v
		case "oom_group_kill":
			e.OOMGroupKill = v
		}
	})
	return e, err
}

// parsePSI reads a pressure file: the cgroup's cpu.pressure, memory.pressure
// and io.pressure, and /proc/pressure/*, which have one shape:
//
//	some avg10=0.00 avg60=0.00 avg300=0.00 total=0
//	full avg10=0.00 avg60=0.00 avg300=0.00 total=0
//
// Only the cumulative totals are kept: they are what rates are made from, and
// they don't depend on when the kernel last averaged.
func parsePSI(b []byte) (PSI, error) {
	var p PSI
	var seenSome bool
	for len(b) > 0 {
		var line []byte
		line, b = nextLine(b)
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		kind, rest, _ := bytes.Cut(line, []byte(" "))
		total, ok := psiTotal(rest)
		if !ok {
			return PSI{}, fmt.Errorf("line %q has no total=", line)
		}
		switch string(kind) {
		case "some":
			p.SomeUsec, seenSome = total, true
		case "full":
			p.FullUsec, p.HasFull = total, true
		default:
			return PSI{}, fmt.Errorf("line %q: unknown kind %q", line, kind)
		}
	}
	if !seenSome {
		return PSI{}, errors.New("no some line")
	}
	return p, nil
}

// psiTotal finds total=<n> among the fields of a pressure line.
func psiTotal(fields []byte) (uint64, bool) {
	for len(fields) > 0 {
		var f []byte
		f, fields, _ = bytes.Cut(fields, []byte(" "))
		if v, ok := bytes.CutPrefix(f, []byte("total=")); ok {
			return atou(v)
		}
	}
	return 0, false
}

// parseIOStat reads io.stat, one line per block device:
//
//	8:0 rbytes=1459200 wbytes=314773504 rios=192 wios=353 dbytes=0 dios=0
//
// and sums the lines of the devices skip doesn't name (nil skips none). It also
// says how many lines the file had. An empty file, which is what a cgroup that
// has done no I/O has, is a zero IO. Other keys on a line (iocost's cost.*)
// are not used.
func parseIOStat(b []byte, skip func(major, minor uint32) bool) (sum IO, devices int, err error) {
	for len(b) > 0 {
		var line []byte
		line, b = nextLine(b)
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		dev, rest, _ := bytes.Cut(line, []byte(" "))
		major, minor, ok := parseDevno(dev)
		if !ok {
			return IO{}, 0, fmt.Errorf("line %q: %q is not MAJOR:MINOR", line, dev)
		}
		devices++
		counted := skip == nil || !skip(major, minor)
		for len(rest) > 0 {
			var field []byte
			field, rest, _ = bytes.Cut(rest, []byte(" "))
			if len(field) == 0 {
				continue // a doubled space
			}
			k, val, ok := bytes.Cut(field, []byte("="))
			if !ok {
				return IO{}, 0, fmt.Errorf("line %q: %q is not key=value", line, field)
			}
			var into *uint64
			switch string(k) {
			case "rbytes":
				into = &sum.ReadBytes
			case "wbytes":
				into = &sum.WriteBytes
			case "rios":
				into = &sum.ReadOps
			case "wios":
				into = &sum.WriteOps
			default:
				continue
			}
			v, ok := atou(val)
			if !ok {
				return IO{}, 0, fmt.Errorf("line %q: %q is not a number", line, field)
			}
			if counted {
				*into += v
			}
		}
	}
	sum.HasOps = true
	return sum, devices, nil
}

// parseDevno parses "MAJOR:MINOR".
func parseDevno(b []byte) (major, minor uint32, ok bool) {
	maj, min, found := bytes.Cut(b, []byte(":"))
	if !found {
		return 0, 0, false
	}
	a, ok1 := atou(maj)
	c, ok2 := atou(min)
	if !ok1 || !ok2 || a > 1<<32-1 || c > 1<<32-1 {
		return 0, 0, false
	}
	return uint32(a), uint32(c), true
}

// absent says err means the file isn't there to read: no such file (the
// controller is off, or the kernel predates it) or a kernel without pressure
// stall information, which refuses the pressure files.
func absent(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.EOPNOTSUPP)
}

// usageReader reads cgroup accounting files through one buffer. It isn't safe
// for concurrent use: one sampler goroutine owns it.
type usageReader struct {
	buf []byte
	// stacked reports whether a block device is built on others, whose I/O the
	// devices beneath it count too. nil: none is.
	stacked func(major, minor uint32) bool
}

// read returns path's contents. They stay valid until the next read.
func (r *usageReader) read(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if cap(r.buf) == 0 {
		r.buf = make([]byte, 0, 8<<10)
	}
	b := r.buf[:0]
	for {
		if len(b) == cap(b) {
			b = append(b, 0)[:len(b)] // grow
		}
		n, err := f.Read(b[len(b):cap(b)])
		b = b[:len(b)+n]
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	r.buf = b
	return b, nil
}

// usage reads the accounting files of the cgroup at dir. Files the kernel
// doesn't have, or hasn't switched on for this cgroup, leave their group nil;
// a file it has but that can't be read or understood is an error alongside
// what could be read. errGone, with nothing else, says the cgroup has been
// removed, which a cgroup that vanishes while it is read looks like too.
func (r *usageReader) usage(dir string) (Usage, error) {
	var u Usage
	var errs []error
	dead := false
	// get returns a file's bytes. A file that isn't there is skipped quietly;
	// the kernel answers ENODEV (or ENOTDIR) for what it is tearing down.
	get := func(file string) ([]byte, bool) {
		b, err := r.read(dir + "/" + file)
		switch {
		case err == nil:
			return b, true
		case errors.Is(err, syscall.ENODEV) || errors.Is(err, syscall.ENOTDIR):
			dead = true
		case file == "cpu.stat" && errors.Is(err, fs.ErrNotExist):
			// every cgroup but the root has one: if it isn't there, nor is the cgroup
			dead = true
		case !absent(err):
			errs = append(errs, fmt.Errorf("%s: %w", file, err))
		}
		return nil, false
	}
	fail := func(file string, err error) { errs = append(errs, fmt.Errorf("%s: %w", file, err)) }

	if b, ok := get("cpu.stat"); ok {
		if c, err := parseCPUStat(b); err != nil {
			fail("cpu.stat", err)
		} else {
			u.CPU = &c
		}
	}

	if b, ok := get("memory.current"); ok {
		if cur, err := parseUint(b); err != nil {
			fail("memory.current", err)
		} else {
			u.Memory = &Memory{Current: cur}
		}
	}
	if u.Memory != nil {
		if b, ok := get("memory.peak"); ok {
			if peak, err := parseUint(b); err != nil {
				fail("memory.peak", err)
			} else {
				u.Memory.Peak, u.Memory.HasPeak = peak, true
			}
		}
		if b, ok := get("memory.stat"); ok {
			if anon, file, err := parseMemoryStat(b); err != nil {
				fail("memory.stat", err)
			} else {
				u.Memory.Anon, u.Memory.File, u.Memory.HasStat = anon, file, true
			}
		}
	}

	if b, ok := get("io.stat"); ok {
		if sum, _, err := parseIOStat(b, r.stacked); err != nil {
			fail("io.stat", err)
		} else {
			u.IO = &sum
		}
	}

	var press Pressure
	for _, p := range []struct {
		file string
		into **PSI
	}{{"cpu.pressure", &press.CPU}, {"memory.pressure", &press.Memory}, {"io.pressure", &press.IO}} {
		if b, ok := get(p.file); ok {
			if psi, err := parsePSI(b); err != nil {
				fail(p.file, err)
			} else {
				*p.into = &psi
			}
		}
	}
	if press != (Pressure{}) {
		u.Pressure = &press
	}

	if b, ok := get("memory.events"); ok {
		if ev, err := parseMemEvents(b); err != nil {
			fail("memory.events", err)
		} else {
			u.Events = &ev
		}
	}
	if dead {
		return Usage{}, errGone
	}
	return u, errors.Join(errs...)
}
