//go:build !unix

package replay

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type commandResult struct {
	exitCode    int
	timedOut    bool
	interrupted bool
}

func (r commandResult) ok() bool { return false }

func (r commandResult) describe(name string, _ time.Duration) string { return name + " did not run" }

// runCommand fails: Elprovo needs process groups, which only macOS and Linux
// give it.
func runCommand(context.Context, string, string, []string, string, time.Duration) (commandResult, error) {
	return commandResult{}, errors.New("elprovo runs commands only on macOS and Linux")
}

func formatDuration(d time.Duration) string { return fmt.Sprint(d) }
