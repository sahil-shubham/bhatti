package krucible

import (
	"path"
	"sort"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

var _ engine.StatsSource = (*Engine)(nil)

// StatsTargets lists what the collector samples: every sandbox's VM, the owners
// whose slices hold a measured scope, and the running gateways.
//
// A cgroup is only reported when the helper is in it right now, checked against
// /proc: a helper that was never placed, was moved away, or whose scope
// doesn't match (adoption failed, or someone moved it) is reported without a
// cgroup, so the collector never reads the daemon's own cgroup, or another
// sandbox's, as this sandbox's. The path is the one /proc shows, so it is right
// on a host and inside a container with its own systemd alike.
//
// It never waits on a transition. A VM's lock is only held briefly, but a
// gateway's is held through its whole start, placement included, so a gateway
// being started or replaced is left out until the next call.
func (e *Engine) StatsTargets() engine.StatsTargets {
	e.mu.RLock()
	vms := make([]*VM, 0, len(e.vms))
	for _, vm := range e.vms {
		vms = append(vms, vm)
	}
	e.mu.RUnlock()

	var t engine.StatsTargets
	owners := map[string]*engine.StatsOwner{}
	// own notes an owner whose slice, at path, holds something measured.
	own := func(slug, userID, slice string) {
		o := owners[slug]
		if o == nil {
			o = &engine.StatsOwner{Owner: slug, Cgroup: slice}
			owners[slug] = o
		}
		if o.UserID == "" {
			o.UserID = userID
		}
	}

	for _, vm := range vms {
		vm.mu.Lock()
		s := engine.StatsSandbox{ID: vm.ID, Owner: vm.cgroupOwner(), UserID: vm.UserID, NetIP: vm.netIP}
		running, scope := vm.Status == "running" && vm.HelperPID > 0, vm.scope
		if running {
			s.PID = vm.HelperPID
		}
		vm.mu.Unlock()

		s.Accounting = e.cg.accounting(placedScope{})
		if running {
			s.Accounting = e.cg.accounting(scope)
			if s.Accounting == engine.AccountingCgroup || s.Accounting == engine.AccountingPartial {
				if ps, err := vmPlacement(s.Owner, s.ID); err == nil {
					s.Cgroup = e.helperCgroup(s.PID, ps)
				}
				if s.Cgroup == "" {
					s.Accounting = engine.AccountingNone
				} else {
					own(s.Owner, s.UserID, path.Dir(s.Cgroup))
				}
			}
		}
		t.Sandboxes = append(t.Sandboxes, s)
	}
	// Keep a listing's owner for the owners that only have a gateway too.
	userOf := map[string]string{}
	for _, s := range t.Sandboxes {
		if s.UserID != "" {
			userOf[s.Owner] = s.UserID
		}
	}

	e.netdMu.Lock()
	insts := make([]*netdInstance, 0, len(e.netds))
	for _, inst := range e.netds {
		insts = append(insts, inst)
	}
	e.netdMu.Unlock()
	for _, inst := range insts {
		if !inst.mu.TryLock() {
			continue
		}
		pid, scope, ctl, key := inst.pid, inst.scope, inst.ctlSock, inst.owner
		inst.mu.Unlock()
		if pid <= 0 {
			continue
		}
		slug := ownerSlug(key)
		n := engine.StatsNetd{Owner: slug, PID: pid, CtlSock: ctl, Accounting: e.cg.accounting(scope)}
		if n.Accounting == engine.AccountingCgroup || n.Accounting == engine.AccountingPartial {
			if ps, err := netdPlacement(slug); err == nil {
				n.Cgroup = e.helperCgroup(pid, ps)
			}
			if n.Cgroup == "" {
				n.Accounting = engine.AccountingNone
			} else {
				own(slug, userOf[slug], path.Dir(n.Cgroup))
			}
		}
		t.Netds = append(t.Netds, n)
	}

	for _, o := range owners {
		t.Owners = append(t.Owners, *o)
	}
	// Ordered, for a listing that reads the same every time it is taken.
	sort.Slice(t.Owners, func(i, j int) bool { return t.Owners[i].Owner < t.Owners[j].Owner })
	sort.Slice(t.Sandboxes, func(i, j int) bool { return t.Sandboxes[i].ID < t.Sandboxes[j].ID })
	sort.Slice(t.Netds, func(i, j int) bool { return t.Netds[i].Owner < t.Netds[j].Owner })
	return t
}

// helperCgroup is the cgroup pid is in if that is ps's scope, else "".
func (e *Engine) helperCgroup(pid int, ps placementSpec) string {
	if !e.cg.placing() {
		return ""
	}
	cg, err := e.cg.procCg(pid)
	if err != nil || !memberOf(cg, ps) {
		return ""
	}
	return cg
}
