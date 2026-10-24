//go:build !unix

package gitrepo

import "os/exec"

func inOwnGroup(*exec.Cmd) {}
