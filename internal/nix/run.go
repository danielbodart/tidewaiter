// Package nix is what Tidewaiter does to Nix: profiles - the stable path a
// unit's ExecStart runs from, and the generations behind it that make a
// rollback exact - and the closure drop-in that lets a confined unit see the
// program its profile points at.
//
// Reads are in-process (readlink, realpath). Writes go through nix-env and
// nix-store, as subprocesses behind a Runner: they are what registers GC
// roots through the Nix daemon, and what an operator's own
// `nix-env --list-generations` agrees with, so they are not reimplemented.
package nix

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Runner runs a program to completion and returns what it wrote to stdout.
//
// An interface so tests answer for nix-env and nix-store without a Nix. A
// program that exits non-zero is an error saying so, with the start of what
// it wrote to stderr, which is where nix-env says why.
type Runner interface {
	Run(ctx context.Context, program string, args ...string) ([]byte, error)
}

// RunnerFunc is a function as a Runner.
type RunnerFunc func(ctx context.Context, program string, args ...string) ([]byte, error)

func (f RunnerFunc) Run(ctx context.Context, program string, args ...string) ([]byte, error) {
	return f(ctx, program, args...)
}

// Exec is the real Runner: os/exec, the program found on PATH.
type Exec struct{}

// stderrLimit is how much of a failing program's stderr goes into its error:
// nix-env's reason is in its first line or two, and a build log is not.
const stderrLimit = 300

func (Exec) Run(ctx context.Context, program string, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	command := exec.CommandContext(ctx, program, args...)
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	var exited *exec.ExitError
	if errors.As(err, &exited) {
		said := strings.TrimSpace(stderr.String())
		if len(said) > stderrLimit {
			said = said[:stderrLimit]
		}
		return nil, fmt.Errorf("%s %s exited with status %d: %s", program, strings.Join(args, " "), exited.ExitCode(), said)
	}
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", program, strings.Join(args, " "), err)
	}
	return stdout.Bytes(), nil
}

// runner is r, or Exec when there is none.
func runner(r Runner) Runner {
	if r == nil {
		return Exec{}
	}
	return r
}
