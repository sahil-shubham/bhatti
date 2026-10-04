//go:build !linux

package main

// confine is Linux-only (setuid + no_new_privs + Landlock). On macOS netd runs
// as the developer's own user, so there is no privilege to drop.
func confine(uid, gid int) error { return nil }
