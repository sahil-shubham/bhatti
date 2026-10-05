package krucible

import "testing"

func TestMountsReturnsRecoveredLaunchSpec(t *testing.T) {
	vm := vmFromRecord(vmRecord{
		ID:       "recovered",
		BaseSpec: VMSpec{Mounts: []VMFsMount{{HostPath: "/srv/shared", ReadOnly: true}}},
	})
	e := &Engine{vms: map[string]*VM{"recovered": vm}}
	mounts, ok := e.Mounts("recovered")
	if !ok || len(mounts) != 1 || mounts[0].HostPath != "/srv/shared" || !mounts[0].ReadOnly {
		t.Fatalf("recovered mounts = %+v, known=%t", mounts, ok)
	}
	mounts[0].HostPath = "/tmp/changed"
	again, _ := e.Mounts("recovered")
	if again[0].HostPath != "/srv/shared" {
		t.Fatalf("caller mutated persisted mount spec: %+v", again)
	}
	if _, ok := e.Mounts("missing"); ok {
		t.Fatal("unknown sandbox reported known mounts")
	}
}
