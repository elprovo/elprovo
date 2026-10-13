// Command elprovo-sim runs local simulations of the outside services that
// Elprovo depends on, behind their real API contracts. See sim/README.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// simulation is one simulated service. Run serves until ctx ends.
type simulation interface {
	Run(ctx context.Context) error
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("elprovo-sim stopped", "error", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("elprovo-sim", flag.ExitOnError)
	dataDir := fs.String("data-dir", ".elprovo-sim", "directory that keeps each simulation's state")
	builders := registerSimulations(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	var sims []simulation
	for _, b := range builders {
		sim, err := b(*dataDir, log)
		if err != nil {
			return err
		}
		if sim != nil {
			sims = append(sims, sim)
		}
	}
	if len(sims) == 0 {
		return errors.New("no simulation is enabled; see -h")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runAll(ctx, sims)
}

// builder builds a simulation after flags are parsed. It returns nil when
// the simulation is disabled.
type builder func(dataDir string, log *slog.Logger) (simulation, error)

// runAll runs every simulation until ctx ends or one of them fails, which
// stops the others.
func runAll(ctx context.Context, sims []simulation) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make(chan error, len(sims))
	var wg sync.WaitGroup
	for _, s := range sims {
		wg.Go(func() {
			if err := s.Run(ctx); err != nil && ctx.Err() == nil {
				errs <- fmt.Errorf("%T: %w", s, err)
				cancel()
			}
		})
	}
	wg.Wait()
	close(errs)
	return errors.Join(collect(errs)...)
}

func collect(errs <-chan error) []error {
	var all []error
	for err := range errs {
		all = append(all, err)
	}
	return all
}
