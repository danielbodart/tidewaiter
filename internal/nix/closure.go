package nix

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ClosureBinder lets a confined unit see the program its profile points at.
//
// NixOS confinement gives a unit a root with only its own closure bound in,
// computed from the unit's definition at evaluation time. A unit whose
// ExecStart runs from a profile references no store path at all, and after a
// rollback the profile points at a path the definition has never heard of - so
// without help both would start in a root with no program in it. Before every
// restart, Tidewaiter rewrites a runtime drop-in binding the target's closure
// and the profile's directory, then reloads systemd.
type ClosureBinder interface {
	// Bind binds target's closure and profile's directory into unit. The
	// caller reloads systemd.
	Bind(ctx context.Context, unit, profile, target string) error
	// Ensure binds as Bind does, but only if unit has no drop-in for profile
	// yet - none at all (after a reboot, or a newly declared unit), or one for
	// a different profile. True when it wrote one; the caller reloads systemd.
	//
	// For `tidewaiter prepare`, so it never overwrites the daemon's: the
	// daemon rewrites the drop-in before every restart, and a prepare that
	// read the profile just before the daemon moved it would otherwise put the
	// old closure back under the new version's restart.
	Ensure(ctx context.Context, unit, profile, target string) (bool, error)
}

// ClosureDropIn is the drop-in's file name: early enough that a unit's own
// later drop-ins can still add to it.
const ClosureDropIn = "50-tidewaiter-closure.conf"

// DefaultRuntimeDir is where runtime unit drop-ins go: gone after a reboot,
// until `tidewaiter prepare` writes them again before anything starts.
const DefaultRuntimeDir = "/run/systemd/system"

// profileLine is the comment line naming the profile a drop-in was written for.
const profileLine = "# Profile: "

// ClosureDropInText is the drop-in's text: the profile's directory (so the
// profile symlink and its generation link resolve inside the root) and every
// path of the closure, read-only, sorted and once each.
func ClosureDropInText(profile string, closure []string) string {
	paths := slices.Clone(closure)
	slices.Sort(paths)
	paths = slices.Compact(paths)
	lines := []string{
		"# Written by tidewaiter before each restart: what the profile points at, and its closure.",
		profileLine + profile,
		"[Service]",
	}
	for _, path := range append([]string{filepath.Dir(profile)}, paths...) {
		lines = append(lines, "BindReadOnlyPaths="+path)
	}
	return strings.Join(lines, "\n") + "\n"
}

// DropInProfile is the profile a drop-in was written for, or "" if it does
// not say.
func DropInProfile(text string) string {
	for line := range strings.SplitSeq(text, "\n") {
		if profile, found := strings.CutPrefix(line, profileLine); found {
			return profile
		}
	}
	return ""
}

// NixClosureBinder takes the closure from `nix-store --query --requisites`
// and writes the drop-in under RuntimeDir/<unit>.d.
type NixClosureBinder struct {
	// DefaultRuntimeDir when empty.
	RuntimeDir string
	Run        Runner
	// The nix-store to run; "nix-store" on PATH when empty.
	NixStore string
	// Names this process's temporary files, so two writers never share one.
	PID int
}

var _ ClosureBinder = NixClosureBinder{}

func (b NixClosureBinder) Bind(ctx context.Context, unit, profile, target string) error {
	file := b.file(unit)
	temporary, err := b.write(ctx, file, profile, target)
	if err != nil {
		return err
	}
	if err := os.Rename(temporary, file); err != nil {
		os.Remove(temporary)
		return err
	}
	return nil
}

func (b NixClosureBinder) Ensure(ctx context.Context, unit, profile, target string) (bool, error) {
	file := b.file(unit)
	existing, err := os.ReadFile(file)
	present := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if present && DropInProfile(string(existing)) == profile {
		return false, nil
	}

	temporary, err := b.write(ctx, file, profile, target)
	if err != nil {
		return false, err
	}
	if present {
		// For another profile: the declaration moved, and what was bound is
		// no use.
		if err := os.Rename(temporary, file); err != nil {
			os.Remove(temporary)
			return false, err
		}
		return true, nil
	}
	// None there: link() rather than rename(), so a drop-in the daemon wrote
	// in the meantime wins.
	defer os.Remove(temporary)
	if err := os.Link(temporary, file); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (b NixClosureBinder) file(unit string) string {
	dir := b.RuntimeDir
	if dir == "" {
		dir = DefaultRuntimeDir
	}
	return filepath.Join(dir, unit+".d", ClosureDropIn)
}

// write puts the drop-in's text in a temporary file of this process's own,
// ready to move into place.
func (b NixClosureBinder) write(ctx context.Context, file, profile, target string) (string, error) {
	closure, err := b.requisites(ctx, target)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return "", err
	}
	pid := b.PID
	if pid == 0 {
		pid = os.Getpid()
	}
	temporary := fmt.Sprintf("%s.%d.tmp", file, pid)
	if err := os.WriteFile(temporary, []byte(ClosureDropInText(profile, closure)), 0o644); err != nil {
		return "", err
	}
	return temporary, nil
}

func (b NixClosureBinder) requisites(ctx context.Context, target string) ([]string, error) {
	program := b.NixStore
	if program == "" {
		program = "nix-store"
	}
	out, err := runner(b.Run).Run(ctx, program, "--query", "--requisites", target)
	if err != nil {
		return nil, err
	}
	var paths []string
	for line := range strings.SplitSeq(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			paths = append(paths, line)
		}
	}
	return paths, nil
}
