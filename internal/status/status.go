// Package status is what the daemon knows right now, written for `tidewaiter
// status` and for scripts: the file, its format, and how it is read back.
//
// Plain JSON in a world-readable directory - /run/tidewaiter, systemd's
// RuntimeDirectory= under the module - so anyone can ask whether the host is in
// use without root, without D-Bus and without the daemon answering. The
// format is the TypeScript Tidewaiter's, field for field.
package status

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/danielbodart/gokit/atomicfile"
	"github.com/danielbodart/tidewaiter/internal/source"
)

// DefaultFile is where the NixOS module has the daemon write its status, and
// where `tidewaiter status` looks.
const DefaultFile = "/run/tidewaiter/status.json"

// Workload is one workload as the daemon last saw it.
type Workload struct {
	Workload string      `json:"workload"`
	Source   source.Name `json:"source"`
	// In use right now, by the last pass or tick; could-not-tell reads as
	// busy.
	Busy bool `json:"busy"`
	// Idle for its whole idle-samples debounce at the last pass.
	Settled bool `json:"settled"`
}

// Reboot is where an automatic reboot stands, when the daemon has one
// configured.
type Reboot struct {
	// The window, as given: "03:00-05:00".
	Window string `json:"window"`
	// Why a reboot is needed ("kernel changed since boot"), or "" when it is
	// not.
	Needed string `json:"needed,omitempty"`
	// What the last pass concluded: "not needed", "waiting for ...",
	// "rebooting, ...".
	Verdict string `json:"verdict"`
}

// Status is what the daemon knows right now.
//
// Written whole after every pass, every activity tick and the start of every
// swap, so a reader sees one consistent moment and never a half-written file.
type Status struct {
	// When this was written.
	Updated time.Time `json:"updated"`
	// Seconds after Updated by which the daemon will have written again: a
	// status older than that was left by a daemon that is no longer running.
	// Zero for a status written before there was such a thing, which never
	// goes stale.
	StaleAfter int `json:"staleAfter,omitempty"`
	// The workloads in use, sorted.
	Busy []string `json:"busy"`
	// The workloads being swapped right now, sorted.
	Swapping  []string   `json:"swapping"`
	Workloads []Workload `json:"workloads"`
	Reboot    *Reboot    `json:"reboot,omitempty"`
}

// MarshalJSON writes Updated in UTC to the millisecond, as JavaScript's
// toISOString did, and the lists as [] rather than null when empty, so a
// script reading busy.length need not guard against null.
func (s Status) MarshalJSON() ([]byte, error) {
	written := struct {
		Updated    string     `json:"updated"`
		StaleAfter int        `json:"staleAfter,omitempty"`
		Busy       []string   `json:"busy"`
		Swapping   []string   `json:"swapping"`
		Workloads  []Workload `json:"workloads"`
		Reboot     *Reboot    `json:"reboot,omitempty"`
	}{s.Updated.UTC().Format("2006-01-02T15:04:05.000Z"), s.StaleAfter, s.Busy, s.Swapping, s.Workloads, s.Reboot}
	if written.Busy == nil {
		written.Busy = []string{}
	}
	if written.Swapping == nil {
		written.Swapping = []string{}
	}
	if written.Workloads == nil {
		written.Workloads = []Workload{}
	}
	return json.Marshal(written)
}

// Sink is where the daemon puts its status; None when nobody asked for one.
type Sink interface {
	Write(Status) error
	// Clear says the daemon is stopping: what it said is no longer true.
	Clear() error
}

// None is the Sink that keeps nothing.
var None Sink = none{}

type none struct{}

func (none) Write(Status) error { return nil }
func (none) Clear() error       { return nil }

// File is a JSON file, replaced whole (atomicfile.Write) so a reader never
// sees it half written.
type File struct {
	Path string
}

func (f File) Write(status Status) error {
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
		return err
	}
	text, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(f.Path, append(text, '\n'), 0o644)
}

func (f File) Clear() error {
	err := os.Remove(f.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Read is the status at path, and false when there is none - no daemon
// writing one.
func Read(path string) (Status, bool, error) {
	text, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Status{}, false, nil
	}
	if err != nil {
		return Status{}, false, err
	}
	var status Status
	if err := json.Unmarshal(text, &status); err != nil {
		return Status{}, false, fmt.Errorf("%s: %w", path, err)
	}
	return status, true, nil
}

// IsStale is whether the daemon that wrote this has stopped writing: one
// killed outside systemd leaves its last status behind, which says nothing
// about now.
func IsStale(status Status, now time.Time) bool {
	return status.StaleAfter > 0 && now.Sub(status.Updated) > time.Duration(status.StaleAfter)*time.Second
}

// IsBusy is whether anything is in use or being swapped: what `tidewaiter
// status` exits 1 for.
func IsBusy(status Status) bool {
	return len(status.Busy) > 0 || len(status.Swapping) > 0
}

// Describe is `tidewaiter status` for a person: a headline, one line per
// workload, and where an automatic reboot stands.
func Describe(status Status, now time.Time) []string {
	list := func(names []string) string { return strings.Join(names, ", ") }
	var lines []string
	switch {
	case len(status.Swapping) > 0:
		lines = append(lines, "Busy: swapping "+list(status.Swapping))
	case len(status.Busy) > 0:
		lines = append(lines, "Busy: "+list(status.Busy)+" in use")
	default:
		lines = append(lines, "Idle: nothing in use, nothing being swapped")
	}
	for _, workload := range status.Workloads {
		state := "idle, not yet for long enough"
		switch {
		case slices.Contains(status.Swapping, workload.Workload):
			state = "swapping"
		case workload.Busy:
			state = "in use"
		case workload.Settled:
			state = "idle"
		}
		lines = append(lines, fmt.Sprintf("  %s (%s)  %s", workload.Workload, workload.Source, state))
	}
	if reboot := status.Reboot; reboot != nil {
		// With nothing needed the verdict is "not needed" - or that it could
		// not tell, which must not read as "not needed".
		if reboot.Needed == "" {
			lines = append(lines, fmt.Sprintf("Automatic reboot (%s): %s", reboot.Window, reboot.Verdict))
		} else {
			lines = append(lines, fmt.Sprintf("Automatic reboot (%s): needed, %s; %s", reboot.Window, reboot.Needed, reboot.Verdict))
		}
	}
	age := max(0, math.Round(now.Sub(status.Updated).Seconds()))
	lines = append(lines, fmt.Sprintf("As of %ds ago", int(age)))
	return lines
}

// Exit codes of `tidewaiter status`.
const (
	Idle    = 0
	Busy    = 1
	Unknown = 2
)

// Command is `tidewaiter status`: print what the running daemon last wrote to
// out, and say Idle, Busy, or Unknown when there is no daemon writing one - no
// file, or one it has stopped writing, however idle that says it is. An error
// is a file that is there but cannot be read.
func Command(path string, now time.Time, out io.Writer) (int, error) {
	current, found, err := Read(path)
	if err != nil {
		return 0, err
	}
	if !found {
		fmt.Fprintf(out, "No status at %s: is Tidewaiter running, with --status-file?\n", path)
		return Unknown, nil
	}
	if IsStale(current, now) {
		fmt.Fprintf(out, "Stale status at %s: is Tidewaiter still running? Its last word was:\n", path)
		fmt.Fprintln(out, strings.Join(Describe(current, now), "\n"))
		return Unknown, nil
	}
	fmt.Fprintln(out, strings.Join(Describe(current, now), "\n"))
	if IsBusy(current) {
		return Busy, nil
	}
	return Idle, nil
}
