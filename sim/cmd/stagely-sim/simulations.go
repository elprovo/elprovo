package main

import (
	"flag"
	"log/slog"

	"github.com/stagely-dev/stagely/sim/internal/aws"
	"github.com/stagely-dev/stagely/sim/internal/azure"
	"github.com/stagely-dev/stagely/sim/internal/digitalocean"
	"github.com/stagely-dev/stagely/sim/internal/gcp"
	"github.com/stagely-dev/stagely/sim/internal/github"
	"github.com/stagely-dev/stagely/sim/internal/hetzner"
	"github.com/stagely-dev/stagely/sim/internal/machines"
	"github.com/stagely-dev/stagely/sim/internal/stripe"
	"github.com/stagely-dev/stagely/sim/internal/vultr"
)

// registerSimulations registers each simulation's flags and returns the
// builders that run after parsing. Each simulation package adds one line.
func registerSimulations(fs *flag.FlagSet) []builder {
	machineOptions := machines.Flags(fs)
	return []builder{
		buildIfEnabled(stripe.Flags(fs)),
		buildIfEnabled(github.Flags(fs)),
		buildIfEnabled(digitalocean.Flags(fs, machineOptions)),
		buildIfEnabled(aws.Flags(fs, machineOptions)),
		buildIfEnabled(gcp.Flags(fs, machineOptions)),
		buildIfEnabled(hetzner.Flags(fs, machineOptions)),
		buildIfEnabled(vultr.Flags(fs, machineOptions)),
		buildIfEnabled(azure.Flags(fs, machineOptions)),
	}
}

func buildIfEnabled[S interface {
	*T
	simulation
}, T any](build func(dataDir string, log *slog.Logger) (S, error)) builder {
	return func(dataDir string, log *slog.Logger) (simulation, error) {
		s, err := build(dataDir, log)
		if err != nil || s == nil {
			return nil, err
		}
		return s, nil
	}
}
