package krucible

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

type identityLookupSystemd struct {
	systemdAPI
	afterLookup func()
}

func (s *identityLookupSystemd) Units(ctx context.Context, pattern string) ([]unitStatus, error) {
	units, err := s.systemdAPI.Units(ctx, pattern)
	if s.afterLookup != nil {
		s.afterLookup()
	}
	return units, err
}

func TestAdoptRevalidatesIdentityBeforeMigration(t *testing.T) {
	for _, phase := range []string{"cgroup lookup", "unit lookup", "owner slice", "scope retry", "argv change"} {
		t.Run(phase, func(t *testing.T) {
			sd := newFakeSystemd()
			m := newTestManager(sd)
			t.Cleanup(m.close)
			ps, pid := testVM(t), adoptee(t)
			var generation atomic.Uint64
			generation.Store(100)
			m.procStart = func(int) (uint64, error) { return generation.Load(), nil }
			var correctArgv atomic.Bool
			correctArgv.Store(true)
			ps.alive = func(pid int) bool { return correctArgv.Load() && pidAlive(pid) }
			scopeAttempts := 0
			sd.onStart = func(name string, _ map[string]any) {
				if name == ps.unit {
					scopeAttempts++
				}
				if name == ps.slice() {
					switch phase {
					case "owner slice":
						generation.Store(200)
					case "argv change":
						correctArgv.Store(false)
					}
				}
			}
			switch phase {
			case "cgroup lookup":
				m.procCg = func(pid int) (string, error) {
					cg, err := sd.procCg(pid)
					generation.Store(200)
					return cg, err
				}
			case "unit lookup":
				m.sd = &identityLookupSystemd{systemdAPI: sd, afterLookup: func() { generation.Store(200) }}
			case "scope retry":
				sd.units[ps.unit] = &fakeUnit{props: map[string]any{}, active: "inactive", refs: 1}
				m.held[ps.unit] = 4242
				lookups := 0
				m.sd = &identityLookupSystemd{systemdAPI: sd, afterLookup: func() {
					lookups++
					if lookups == 2 {
						generation.Store(200)
					}
				}}
			}

			if _, err := m.adopt(context.Background(), pid, ps, placedScope{}); err == nil {
				t.Fatal("adopted a different process incarnation")
			}
			wantAttempts := 0
			if phase == "scope retry" {
				wantAttempts = 1 // the rejected attempt before identity changed
			}
			if scopeAttempts != wantAttempts {
				t.Errorf("scope creation attempts = %d, want %d before identity changed", scopeAttempts, wantAttempts)
			}
			if cg, _ := sd.procCg(pid); memberOf(cg, ps) {
				t.Fatalf("replacement process was migrated into %q", cg)
			}
			if !pidAlive(pid) || len(sd.calls("stop")) != 0 {
				t.Fatal("refusing adoption stopped the process")
			}
		})
	}
}

func TestRecoveryPinsIdentityBeforeHelperProbes(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprintf("changed=%t", changed), func(t *testing.T) {
			sd := newFakeSystemd()
			e := recoverFixture(t, sd)
			var generation atomic.Uint64
			generation.Store(100)
			e.cg.procStart = func(int) (uint64, error) { return generation.Load(), nil }
			spec, ctl := sandboxPaths(t, e, testID)
			pid := gatedStandin(t, spec)
			ps := recordRunning(t, e, testID, pid, placedScope{})
			ln, err := net.Listen("unix", ctl)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { ln.Close() })
			t.Cleanup(e.Shutdown)
			go func() {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				defer c.Close()
				if line, _ := bufio.NewReader(c).ReadString('\n'); strings.TrimSpace(line) == "STATUS" {
					if changed {
						generation.Store(200)
					}
					fmt.Fprintln(c, "OK paused")
				}
			}()

			if err := e.recover(); err != nil {
				t.Fatal(err)
			}
			vm := e.vms[testID]
			vm.mu.Lock()
			status, thermal, scope := vm.Status, vm.Thermal, vm.scope
			vm.mu.Unlock()
			if status != "running" || thermal != "warm" || !pidAlive(pid) {
				t.Fatalf("recovery did not preserve the guest: status %s, thermal %s, alive %v", status, thermal, pidAlive(pid))
			}
			if changed {
				if scope != (placedScope{}) {
					t.Fatalf("a PID changed during the helper probe was placed: %+v", scope)
				}
				if cg, _ := sd.procCg(pid); memberOf(cg, ps) {
					t.Fatalf("the replacement process was migrated to %q", cg)
				}
			} else if scope != (placedScope{unit: ps.unit, partial: true}) {
				t.Fatalf("unchanged helper was not adopted: %+v", scope)
			}
		})
	}
}

func TestProcStartTimeIsStable(t *testing.T) {
	first, err := procStartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	second, err := procStartTime(os.Getpid())
	if err != nil || first != second {
		t.Fatalf("one process changed start time: %d, %d (%v)", first, second, err)
	}
	if _, err := procStartTime(-1); err == nil {
		t.Fatal("nonexistent PID returned a start time")
	}
}
