// Package vm holds the build context of the simulated cloud VM image: its
// Dockerfile, its systemd units and the source of elprovo-vm-init, which the
// image build compiles on its own, so vminit imports only the standard
// library.
package vm

import "embed"

// Context is the image's Docker build context. It also holds vminit's test
// files, which the build does not need.
//
//go:embed Dockerfile units vminit/*.go
var Context embed.FS
