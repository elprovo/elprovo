// Command elprovo replays the proofs that come with a change. See README.md.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/elprovo/elprovo-next/internal/cli"
)

// version is set at release builds with -ldflags "-X main.version=v0.1.0".
var version = ""

func main() {
	dir, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "elprovo: %v\n", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	status := cli.Run(ctx, buildVersion(), os.Args[1:], cli.Env{
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Environ: os.Environ(),
		Dir:     dir,
	})
	stop()
	os.Exit(status)
}

// buildVersion returns the release version, the module version that go
// install recorded, or the commit the binary was built from.
func buildVersion() string {
	if version != "" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	revision, modified := "", false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if revision == "" {
		return "dev"
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	if modified {
		revision += "-modified"
	}
	return "dev-" + revision
}
