package nix

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/danielbodart/gokit/atomicfile"
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
}

var _ ClosureBinder = NixClosureBinder{}

func (b NixClosureBinder) Bind(ctx context.Context, unit, profile, target string) error {
	file := b.file(unit)
	text, err := b.text(ctx, file, target, profile)
	if err != nil {
		return err
	}
	return atomicfile.Write(file, text, 0o644)
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

	text, err := b.text(ctx, file, target, profile)
	if err != nil {
		return false, err
	}
	if present {
		// For another profile: the declaration moved, and what was bound is
		// no use.
		if err := atomicfile.Write(file, text, 0o644); err != nil {
			return false, err
		}
		return true, nil
	}
	// None there: created only if it still is not, so a drop-in the daemon
	// wrote in the meantime wins.
	if err := atomicfile.Create(file, text, 0o644); err != nil {
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

// text is the drop-in binding target's closure for profile, with the
// directory it goes in made ready for it.
func (b NixClosureBinder) text(ctx context.Context, file, target, profile string) ([]byte, error) {
	closure, err := b.requisites(ctx, target)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return nil, err
	}
	return []byte(ClosureDropInText(profile, closure)), nil
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
