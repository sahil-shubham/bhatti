//go:build linux

package krucible

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

// hostCgroups is how this Linux host answers the questions behind the "auto"
// mode: is systemd running the machine, is it a cgroup v2 machine, and is the
// system bus there to talk to it.
func hostCgroups() cgroupHost {
	return cgroupHost{pid1: pid1Name, cgroup2: cgroup2Mounted, dial: dialSystemd, procCg: procCgroupPath}
}

func pid1Name() (string, error) {
	b, err := os.ReadFile("/proc/1/comm")
	return strings.TrimSpace(string(b)), err
}

// cgroup2Mounted: the unified hierarchy at /sys/fs/cgroup. A hybrid host mounts
// a tmpfs there with cgroup2 beneath it, which systemd's scopes can't be read
// from without a second convention, so it doesn't count.
func cgroup2Mounted() (bool, error) {
	var st unix.Statfs_t
	if err := unix.Statfs("/sys/fs/cgroup", &st); err != nil {
		return false, err
	}
	return int64(st.Type) == unix.CGROUP2_SUPER_MAGIC, nil
}

// procCgroupPath is the cgroup v2 path of pid: the "0::<path>" line of
// /proc/<pid>/cgroup, relative to this process's cgroup namespace.
func procCgroupPath(pid int) (string, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cgroup")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if path, ok := strings.CutPrefix(line, "0::"); ok {
			return path, nil
		}
	}
	return "", errors.New("no cgroup v2 entry")
}

const (
	sdDest    = "org.freedesktop.systemd1"
	sdManager = "org.freedesktop.systemd1.Manager"
	sdPath    = dbus.ObjectPath("/org/freedesktop/systemd1")
)

// busSystemd is systemd's manager over the system bus: one connection for the
// daemon, dialled on first use and again, once per call, if the bus went away
// (dbus-daemon restarts must not leave every later placement failing).
type busSystemd struct {
	mu   sync.Mutex
	conn *dbus.Conn
}

func dialSystemd(ctx context.Context) (systemdAPI, error) {
	b := &busSystemd{}
	if _, err := b.connection(ctx); err != nil {
		return nil, err
	}
	return b, nil
}

// connection returns the live connection, dialling one if there is none. The
// dial has no deadline of its own, so it runs apart and is abandoned when ctx
// ends; a late connection is closed.
func (b *busSystemd) connection(ctx context.Context) (*dbus.Conn, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn != nil && b.conn.Connected() {
		return b.conn, nil
	}
	type dialed struct {
		conn *dbus.Conn
		err  error
	}
	ch := make(chan dialed, 1)
	go func() {
		conn, err := dbus.ConnectSystemBus()
		ch <- dialed{conn, err}
	}()
	select {
	case d := <-ch:
		if d.err != nil {
			return nil, d.err
		}
		b.conn = d.conn
		return d.conn, nil
	case <-ctx.Done():
		go func() {
			if d := <-ch; d.conn != nil {
				d.conn.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

// drop forgets conn if it is still the current connection.
func (b *busSystemd) drop(conn *dbus.Conn) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn == conn {
		b.conn = nil
	}
	conn.Close()
}

// call invokes method on obj and stores the reply in out. An error from systemd
// itself is the answer; a failure of the connection is retried once on a fresh
// one.
func (b *busSystemd) call(ctx context.Context, obj dbus.ObjectPath, method string, out []any, args ...any) error {
	for attempt := 0; ; attempt++ {
		conn, err := b.connection(ctx)
		if err != nil {
			return err
		}
		err = conn.Object(sdDest, obj).CallWithContext(ctx, method, 0, args...).Store(out...)
		var remote dbus.Error
		if err == nil || errors.As(err, &remote) {
			return mapBusError(err)
		}
		if ctx.Err() != nil || attempt > 0 {
			return err
		}
		b.drop(conn)
	}
}

func mapBusError(err error) error {
	var remote dbus.Error
	if !errors.As(err, &remote) {
		return err
	}
	switch remote.Name {
	case "org.freedesktop.systemd1.NoSuchUnit":
		return fmt.Errorf("%w: %v", errNoSuchUnit, err)
	case "org.freedesktop.systemd1.UnitExists":
		return fmt.Errorf("%w: %v", errUnitExists, err)
	case "org.freedesktop.systemd1.NotReferenced":
		return fmt.Errorf("%w: %v", errUnitNotReferenced, err)
	}
	return err
}

func (b *busSystemd) StartTransientUnit(ctx context.Context, name string, props []unitProp) error {
	type property struct {
		Name  string
		Value dbus.Variant
	}
	type aux struct {
		Name       string
		Properties []property
	}
	sent := make([]property, len(props))
	for i, p := range props {
		sent[i] = property{p.Name, dbus.MakeVariant(p.Value)}
	}
	var job dbus.ObjectPath
	return b.call(ctx, sdPath, sdManager+".StartTransientUnit", []any{&job}, name, "replace", sent, []aux{})
}

func (b *busSystemd) StopUnit(ctx context.Context, name string) error {
	var job dbus.ObjectPath
	return b.call(ctx, sdPath, sdManager+".StopUnit", []any{&job}, name, "replace")
}

func (b *busSystemd) Units(ctx context.Context, pattern string) ([]unitStatus, error) {
	var rows []struct {
		Name, Description, Load, Active, Sub, Followed string
		Path                                           dbus.ObjectPath
		JobID                                          uint32
		JobType                                        string
		JobPath                                        dbus.ObjectPath
	}
	if err := b.call(ctx, sdPath, sdManager+".ListUnitsByPatterns", []any{&rows}, []string{}, []string{pattern}); err != nil {
		return nil, err
	}
	units := make([]unitStatus, len(rows))
	for i, r := range rows {
		units[i] = unitStatus{Name: r.Name, Description: r.Description, Active: r.Active}
	}
	return units, nil
}

func (b *busSystemd) RefUnit(ctx context.Context, name string) error {
	return b.call(ctx, sdPath, sdManager+".RefUnit", nil, name)
}

func (b *busSystemd) UnrefUnit(ctx context.Context, name string) error {
	return b.call(ctx, sdPath, sdManager+".UnrefUnit", nil, name)
}

// Usage reads the figures systemd keeps for a scope. After a scope's last process
// exits systemd still has its CPU time, peak memory and I/O totals for as long as
// the unit stays loaded (a reference holds it); MemoryCurrent is no longer one of
// them. A figure systemd lacks comes back as the maximum uint64.
func (b *busSystemd) Usage(ctx context.Context, name string) (unitUsage, error) {
	var path dbus.ObjectPath
	if err := b.call(ctx, sdPath, sdManager+".GetUnit", []any{&path}, name); err != nil {
		return unitUsage{}, err
	}
	var props map[string]dbus.Variant
	if err := b.call(ctx, path, "org.freedesktop.DBus.Properties.GetAll", []any{&props}, "org.freedesktop.systemd1.Scope"); err != nil {
		return unitUsage{}, err
	}
	figure := func(key string) int64 {
		v, ok := props[key].Value().(uint64)
		if !ok || v > 1<<62 { // ^uint64(0): systemd has no number
			return -1
		}
		return int64(v)
	}
	result, _ := props["Result"].Value().(string)
	return unitUsage{
		CPUNanos: figure("CPUUsageNSec"), MemoryPeak: figure("MemoryPeak"),
		IORead: figure("IOReadBytes"), IOWrite: figure("IOWriteBytes"), Result: result,
	}, nil
}

func (b *busSystemd) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn == nil {
		return nil
	}
	err := b.conn.Close()
	b.conn = nil
	return err
}
