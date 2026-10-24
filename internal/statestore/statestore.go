// Package statestore is what the systemd source must remember across its own
// restarts, per unit, kept in a file under the state directory.
//
// Unlike Docker's in-memory state, this is persisted: a rollback leaves the
// failed version as a store path the module still asks for, and a forgotten
// pin would retry it - and restart the unit under its players' feet - once on
// every daemon restart. The idle streak is not here: after a restart it counts
// up again, which only ever delays.
//
// The file's format is the TypeScript Tidewaiter's, field for field, so an
// existing install's pins survive the upgrade.
package statestore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/danielbodart/gokit/atomicfile"
)

// FileName is the store's file under the state directory.
const FileName = "systemd.json"

// UnitMemory is one unit's memory.
type UnitMemory struct {
	// The desired path whose update failed its health check; left alone until
	// desired moves.
	Pinned string `json:"pinned,omitempty"`
	// The desired path swap steps have been failing on, and how many times
	// running.
	Failing      string `json:"failing,omitempty"`
	SwapFailures int    `json:"swapFailures,omitempty"`
	// The profile generation last known good: committed by a healthy swap, or
	// the one already running when the unit was first seen healthy. What crash
	// recovery rolls back to.
	Good *int `json:"good,omitempty"`
	// A swap under way, written before anything is changed and cleared once it
	// commits or is undone, so a Tidewaiter that dies in between finds out on
	// its next start that the profile may point at a version that never ran,
	// or ran but was never gated, rather than taking it for a baseline.
	Swapping *SwapRecord `json:"swapping,omitempty"`
}

// SwapRecord is where a swap started from, where it was going, and the
// invocation it was replacing.
type SwapRecord struct {
	// The profile generation the swap left; nil for a profile it created.
	From *int `json:"from,omitempty"`
	// That generation's store path, for the closure drop-in.
	FromTarget string `json:"fromTarget,omitempty"`
	To         string `json:"to"`
	// The unit's invocation before the restart: unchanged means it never
	// restarted.
	InvocationID string `json:"invocationId"`
}

// Memories is every unit's memory, by unit name.
type Memories map[string]UnitMemory

// Store loads and saves Memories.
type Store interface {
	Load() (Memories, error)
	Save(Memories) error
}

// File is a JSON file, written whole, atomically and durably
// (atomicfile.Write), so a crash or a power cut mid-write leaves the previous
// state rather than half of one. A file that is not there is no memories; one that cannot be parsed is
// an error, because silently forgetting pins is exactly what this file exists
// to stop.
type File struct {
	Path string
}

func (f File) Load() (Memories, error) {
	text, err := os.ReadFile(f.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return Memories{}, nil
	}
	if err != nil {
		return nil, err
	}
	if trimmed := bytes.TrimSpace(text); len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, fmt.Errorf("%s is not a JSON object", f.Path)
	}
	memories := Memories{}
	if err := json.Unmarshal(text, &memories); err != nil {
		return nil, fmt.Errorf("%s: %w", f.Path, err)
	}
	return memories, nil
}

func (f File) Save(memories Memories) error {
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
		return err
	}
	if memories == nil {
		memories = Memories{}
	}
	text, err := json.MarshalIndent(memories, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(f.Path, append(text, '\n'), 0o644)
}
