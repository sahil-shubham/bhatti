// Package confine confines bhatti-vmm on Linux before any of its own code runs.
//
// Importing it is the whole API: the daemon names, in a policy file whose
// path it puts in krucible.VMMPolicyEnv, the unprivileged identity the helper
// runs as and the files it may touch (krucible.VMMPolicy), and a C
// constructor applies that as the process starts. Elsewhere, and when the
// variable is unset (`bhatti-vmm capabilities`, `check-checkpoint`), it does
// nothing.
package confine
