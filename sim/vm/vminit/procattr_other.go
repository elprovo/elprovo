//go:build !linux

package main

import "syscall"

// processAttributes runs a unit's process in its own process group, as its
// user. Ambient capabilities exist only on Linux, where machines run; other
// systems run the unit tests.
func processAttributes(cred *syscall.Credential, _ []uintptr) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Credential: cred}
}
