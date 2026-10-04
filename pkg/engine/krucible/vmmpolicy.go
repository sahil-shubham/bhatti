package krucible

import (
	"bytes"
	"strconv"
)

// VMMPolicyEnv names the file holding a bhatti-vmm's VMMPolicy. The helper
// applies it before any of its Go or libkrun code runs (cmd/vmm/internal/
// confine); unset, the helper runs unconfined, as `capabilities` and
// `check-checkpoint` do.
const VMMPolicyEnv = "BHATTI_VMM_CONFINE"

// VMMPolicy is what a confined bhatti-vmm may do: the identity it drops to,
// the capabilities it keeps, and the only paths it may open. Like VMSpec, it
// is a contract between the daemon (which writes it) and the helper.
type VMMPolicy struct {
	// UID and GID are the identity to drop to (both, or neither: 0 keeps the
	// one the helper was started with, which is only allowed when that isn't
	// root).
	UID, GID uint32
	Groups   []uint32 // supplementary groups
	Caps     []uint   // capabilities kept (CAP_* numbers); normally none
	Rules    []VMMRule
}

// VMMRule allows Access beneath Path (or on it, for a file). Access is a set
// of Landlock filesystem rights; the helper drops those its kernel doesn't
// know, and those that don't apply to a file.
type VMMRule struct {
	Path   string
	Access uint64
}

// Landlock filesystem rights (linux/landlock.h), for VMMRule.Access.
const (
	LandlockExecute    uint64 = 1 << 0
	LandlockWriteFile  uint64 = 1 << 1
	LandlockReadFile   uint64 = 1 << 2
	LandlockReadDir    uint64 = 1 << 3
	LandlockRemoveDir  uint64 = 1 << 4
	LandlockRemoveFile uint64 = 1 << 5
	LandlockMakeChar   uint64 = 1 << 6
	LandlockMakeDir    uint64 = 1 << 7
	LandlockMakeReg    uint64 = 1 << 8
	LandlockMakeSock   uint64 = 1 << 9
	LandlockMakeFifo   uint64 = 1 << 10
	LandlockMakeBlock  uint64 = 1 << 11
	LandlockMakeSym    uint64 = 1 << 12
	LandlockRefer      uint64 = 1 << 13 // ABI 2
	LandlockTruncate   uint64 = 1 << 14 // ABI 3
	LandlockIoctlDev   uint64 = 1 << 15 // ABI 5
)

// Encode renders p as the helper reads it: NUL-terminated "<key> <value>"
// records, so a path may hold any byte but NUL.
func (p VMMPolicy) Encode() []byte {
	var b bytes.Buffer
	rec := func(key, value string) {
		b.WriteString(key)
		b.WriteByte(' ')
		b.WriteString(value)
		b.WriteByte(0)
	}
	if p.UID != 0 || p.GID != 0 {
		rec("uid", strconv.FormatUint(uint64(p.UID), 10))
		rec("gid", strconv.FormatUint(uint64(p.GID), 10))
	}
	for _, g := range p.Groups {
		rec("group", strconv.FormatUint(uint64(g), 10))
	}
	for _, c := range p.Caps {
		rec("cap", strconv.FormatUint(uint64(c), 10))
	}
	for _, r := range p.Rules {
		rec("path", strconv.FormatUint(r.Access, 16)+" "+r.Path)
	}
	return b.Bytes()
}
