// Package confine confines bhatti-vmm on Linux before any of its own code runs.
//
// Importing it is the whole API: the daemon passes a krucible.VMMPolicy file
// through krucible.VMMPolicyEnv, and a C constructor drops identity and
// capabilities, sets no_new_privs, restricts filesystem/network access with
// Landlock, and installs seccomp before Go/libkrun starts threads. VM runs
// and check-checkpoint get policies; capabilities and non-root callers without
// a policy run unconfined.
package confine
