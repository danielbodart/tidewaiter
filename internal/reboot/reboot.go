// Package reboot decides when the machine may reboot itself: inside a daily
// window, when the installed NixOS needs it, and only once nothing is in use.
//
// What is pure - the window and the verdict - is here, with the boot check
// that reads the filesystem. Asking logind for the reboot is the systemd
// side's, behind Rebooter.
package reboot

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Window is a daily stretch of local time, in minutes since midnight.
//
// End may be 1440 (24:00), and may be before Start, for a window that crosses
// midnight: 23:00-01:00 is the last hour of one day and the first of the next.
type Window struct {
	Start, End int
}

var hhmm = regexp.MustCompile(`^([01][0-9]|2[0-3]):([0-5][0-9])$`)

// ParseWindow reads "03:00-05:00". 24:00 is allowed as an end; an empty window
// is refused.
func ParseWindow(text string) (Window, error) {
	notAWindow := fmt.Errorf("'%s' is not a window: give HH:MM-HH:MM, such as 03:00-05:00", text)
	parts := strings.Split(text, "-")
	if len(parts) != 2 {
		return Window{}, notAWindow
	}
	minutes := func(time string, end bool) (int, bool) {
		if end && time == "24:00" {
			return 24 * 60, true
		}
		match := hhmm.FindStringSubmatch(time)
		if match == nil {
			return 0, false
		}
		hours, _ := strconv.Atoi(match[1])
		minutes, _ := strconv.Atoi(match[2])
		return hours*60 + minutes, true
	}
	start, ok := minutes(parts[0], false)
	if !ok {
		return Window{}, notAWindow
	}
	end, ok := minutes(parts[1], true)
	if !ok {
		return Window{}, notAWindow
	}
	if start == end {
		return Window{}, fmt.Errorf("'%s' is an empty window; 00:00-24:00 is the whole day", text)
	}
	return Window{Start: start, End: end}, nil
}

// Contains is whether t, in its own location's time of day, is inside the
// window: start inclusive, end exclusive.
func (w Window) Contains(t time.Time) bool {
	minute := t.Hour()*60 + t.Minute()
	if w.Start < w.End {
		return minute >= w.Start && minute < w.End
	}
	return minute >= w.Start || minute < w.End
}

// BootCheck says whether the machine needs a reboot to run what is installed.
type BootCheck interface {
	// Why a reboot is needed, or "" when it is not. An error when it cannot
	// tell.
	Needed(ctx context.Context) (string, error)
}

// Rebooter reboots the machine. An error is a refusal - someone else's block
// inhibitor, or no logind - and the next pass in the window asks again.
type Rebooter interface {
	Reboot(ctx context.Context) error
}

// bootFiles are what nixpkgs' system.autoUpgrade compares to decide that a
// switch is not enough.
var bootFiles = []string{"initrd", "kernel", "kernel-modules"}

// NixOS is the test system.autoUpgrade makes with allowReboot: the booted
// system's initrd, kernel and kernel modules against the installed system's.
// A difference is something only a reboot puts in place; everything else a
// nightly switch already has.
//
// Both paths are fields, so a test can stand a different "installed" system
// up without installing one.
type NixOS struct {
	// /run/booted-system.
	Booted string
	// /nix/var/nix/profiles/system.
	Current string
}

func (n NixOS) Needed(context.Context) (string, error) {
	// Neither system there at all is "cannot tell", never "everything
	// changed".
	for _, system := range []string{n.Booted, n.Current} {
		if _, err := filepath.EvalSymlinks(system); err != nil {
			return "", fmt.Errorf("cannot read %s (%w)", system, err)
		}
	}
	var changed []string
	for _, name := range bootFiles {
		was, err := resolved(filepath.Join(n.Booted, name))
		if err != nil {
			return "", err
		}
		is, err := resolved(filepath.Join(n.Current, name))
		if err != nil {
			return "", err
		}
		if was != is {
			changed = append(changed, name)
		}
	}
	if len(changed) == 0 {
		return "", nil
	}
	return strings.Join(changed, ", ") + " changed since boot", nil
}

// resolved is where a path finally leads, or "" when it leads nowhere - a
// system with no initrd.
func resolved(path string) (string, error) {
	target, err := filepath.EvalSymlinks(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return target, err
}

// Inputs is what stands between the machine and a reboot it needs.
type Inputs struct {
	// Why a reboot is needed; "" when it is not.
	Needed   string
	InWindow bool
	// In use right now.
	Busy []string
	// Idle, but not yet for their idle-samples debounce - or never sampled.
	Unsettled []string
	Swapping  []string
}

// Kind is what Decide concluded.
type Kind int

const (
	NotNeeded Kind = iota
	Wait
	Reboot
)

// Verdict is Decide's conclusion, and for a Wait what it waits for.
type Verdict struct {
	Kind   Kind
	Reason string
}

// Decide reboots only when one is needed, inside the window, with nothing
// swapping and everything idle for as long as a swap would wait. A window that
// is busy throughout simply passes; the next day's is tried the same way.
func Decide(in Inputs) Verdict {
	list := func(names []string) string {
		sorted := slices.Clone(names)
		slices.Sort(sorted)
		return strings.Join(sorted, ", ")
	}
	switch {
	case in.Needed == "":
		return Verdict{Kind: NotNeeded}
	case !in.InWindow:
		return Verdict{Kind: Wait, Reason: "waiting for the window"}
	case len(in.Swapping) > 0:
		return Verdict{Kind: Wait, Reason: "waiting: swapping " + list(in.Swapping)}
	case len(in.Busy) > 0:
		return Verdict{Kind: Wait, Reason: "waiting: " + list(in.Busy) + " in use"}
	case len(in.Unsettled) > 0:
		return Verdict{Kind: Wait, Reason: "waiting: " + list(in.Unsettled) + " not idle for long enough yet"}
	}
	return Verdict{Kind: Reboot}
}

// Defaults for NixOS, where the module and the CLI find the two systems.
var Defaults = NixOS{Booted: "/run/booted-system", Current: "/nix/var/nix/profiles/system"}
