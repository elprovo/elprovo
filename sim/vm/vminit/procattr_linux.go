package main

import "syscall"

// processAttributes runs a unit's process in its own process group, as its
// user, with its ambient capabilities.
func processAttributes(cred *syscall.Credential, ambientCaps []uintptr) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Credential: cred, AmbientCaps: ambientCaps}
}
