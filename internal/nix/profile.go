package nix

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Profiles is Nix profiles as the systemd source uses them.
//
// A profile P is a symlink to P-<N>-link, which links to a store path. Every
// generation link is a GC root, so whatever a rollback goes back to is still
// in the store. Generations are numbered from 1, so 0 is "no profile".
type Profiles interface {
	// Current is the store path the profile points at now, or "" if there is
	// no profile.
	Current(ctx context.Context, profile string) (string, error)
	// Generation is the current generation number, or 0 if there is no profile.
	Generation(ctx context.Context, profile string) (int, error)
	// Target is the store path a generation points at, or "" if it is gone.
	Target(ctx context.Context, profile string, generation int) (string, error)
	// Set points the profile at a store path, as a new generation.
	Set(ctx context.Context, profile, storePath string) error
	// SwitchGeneration points the profile back at an existing generation.
	SwitchGeneration(ctx context.Context, profile string, generation int) error
	// DeleteGeneration deletes one generation, never the current one.
	DeleteGeneration(ctx context.Context, profile string, generation int) error
	// Prune keeps the current generation and the `keep` newest before it,
	// and deletes the rest.
	Prune(ctx context.Context, profile string, keep int) error
}

// GenerationOf is the generation number in a profile's link name:
// bedrock-12-link is 12. 0 for anything else, a store path included.
//
// Pure, for the tests and for anything reading a profile without a Nix to
// ask.
func GenerationOf(profile, link string) int {
	prefix := filepath.Base(profile) + "-"
	name := filepath.Base(link)
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, "-link") {
		return 0
	}
	digits := strings.TrimSuffix(strings.TrimPrefix(name, prefix), "-link")
	if digits == "" || strings.Trim(digits, "0123456789") != "" {
		return 0
	}
	number, err := strconv.Atoi(digits)
	if err != nil || number < 1 {
		return 0
	}
	return number
}

// NixProfiles is profiles through nix-env, which every Nix install has and
// whose generations are what `nix-env --list-generations` shows an operator.
//
// Reads are two readlinks; writes go through nix-env, which with
// NIX_REMOTE=daemon registers the GC roots through the Nix daemon. A rollback
// is a switch to the generation recorded before the swap, not `--rollback`:
// after a re-`--set` of a path that already had a generation, "the one before
// the current" is not necessarily the one we left.
type NixProfiles struct {
	Run Runner
	// The nix-env to run; "nix-env" on PATH when empty.
	NixEnv string
}

var _ Profiles = NixProfiles{}

func (p NixProfiles) Current(_ context.Context, profile string) (string, error) {
	target, err := filepath.EvalSymlinks(profile)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return target, err
}

func (p NixProfiles) Generation(_ context.Context, profile string) (int, error) {
	link, err := os.Readlink(profile)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	generation := GenerationOf(profile, link)
	if generation == 0 {
		return 0, fmt.Errorf("%s links to '%s', which is not a profile generation", profile, link)
	}
	return generation, nil
}

func (p NixProfiles) Target(ctx context.Context, profile string, generation int) (string, error) {
	return p.Current(ctx, fmt.Sprintf("%s-%d-link", profile, generation))
}

func (p NixProfiles) Set(ctx context.Context, profile, storePath string) error {
	// nix-env will not create the profile's directory ("cannot read directory").
	if err := os.MkdirAll(filepath.Dir(profile), 0o755); err != nil {
		return err
	}
	return p.nixEnv(ctx, "--profile", profile, "--set", storePath)
}

func (p NixProfiles) SwitchGeneration(ctx context.Context, profile string, generation int) error {
	return p.nixEnv(ctx, "--profile", profile, "--switch-generation", strconv.Itoa(generation))
}

func (p NixProfiles) DeleteGeneration(ctx context.Context, profile string, generation int) error {
	return p.nixEnv(ctx, "--profile", profile, "--delete-generations", strconv.Itoa(generation))
}

func (p NixProfiles) Prune(ctx context.Context, profile string, keep int) error {
	// `+N` keeps the N newest generations by number, and the current one is
	// never deleted; the failed generations a rollback leaves are deleted by
	// the rollback itself, so the newest are the good ones.
	return p.nixEnv(ctx, "--profile", profile, "--delete-generations", fmt.Sprintf("+%d", keep+1))
}

func (p NixProfiles) nixEnv(ctx context.Context, args ...string) error {
	program := p.NixEnv
	if program == "" {
		program = "nix-env"
	}
	_, err := runner(p.Run).Run(ctx, program, args...)
	return err
}
