// Package source is a kind of thing Tidewaiter keeps up to date - Docker
// containers, systemd units - as the daemon sees it.
//
// Unlike Portical's sources, which only describe desired state for a shared
// reconcile, a Tidewaiter source also acts - how a container is swapped has
// nothing in common with how a unit is - so a source runs its own pass, built
// from the shared pieces (decide, detect, health).
package source

import (
	"context"
	"fmt"

	"github.com/danielbodart/tidewaiter/internal/decide"
	"github.com/danielbodart/tidewaiter/internal/detect"
)

// Name is which source: "docker" or "systemd".
type Name string

const (
	Docker  Name = "docker"
	Systemd Name = "systemd"
)

// Names is every source, in the order the usage lists them.
var Names = []Name{Docker, Systemd}

// ParseName is the source called name, or an error naming the ones there are.
func ParseName(name string) (Name, error) {
	for _, known := range Names {
		if string(known) == name {
			return known, nil
		}
	}
	return "", fmt.Errorf("'%s' is not a source (docker, systemd)", name)
}

// Pass is what a source is handed for one pass.
//
// The daemon owns everything a pass shares across sources - one conntrack
// dump, and the record of what is being swapped - and passes it in, so two
// sources never dump the table twice.
type Pass struct {
	// The host conntrack table, dumped once per pass and shared by every
	// source.
	//
	// Nil when the dump failed - "could not tell", which a detector that
	// needs the table must read as busy, never as an empty table. An empty,
	// non-nil table is a dump that found no flows.
	Conntrack []detect.ConntrackEntry
	// Hold marks workload as being swapped until the end of the pass.
	//
	// Called before any swap starts, so `tidewaiter status` says so from that
	// moment, and an automatic reboot never lands mid-swap. It returns once
	// the status is written; a status that could not be written is logged by
	// the daemon, and the swap goes ahead.
	Hold func(workload string)
}

// Outcome is what one source concluded about one workload this pass.
type Outcome struct {
	// "chandlery-bedrock.service", or a container name. Unique within a
	// source.
	Workload string
	Action   decide.Action
	// The activity verdict this pass, fail-safe (could-not-tell reads as
	// busy).
	//
	// Nil when the pass never got as far as sampling - a registry that did
	// not answer, a unit systemd could not describe. The daemon then keeps
	// what it last knew about the workload rather than guessing either way.
	Busy *bool
	// Idle for the workload's whole idle-samples debounce this pass - what an
	// automatic reboot waits for, as a swap does. False for a workload that
	// was not sampled, or has just been swapped: it holds a reboot off.
	Settled bool
}

// Sampled is an Outcome's Busy for a workload that was sampled.
func Sampled(busy bool) *bool { return &busy }

// Activity is one workload's activity, for the faster tick between passes.
type Activity struct {
	Workload string
	Busy     bool
}

// Source is a kind of workload Tidewaiter keeps up to date.
//
// The daemon calls one method at a time, never two at once, so a source needs
// no locking of its own for the daemon's sake.
type Source interface {
	Name() Name
	// Once reconciles every workload once. One workload failing never fails
	// the pass: it is logged, and reported with a keep action and no Busy. An
	// error is for a pass that could not see its workloads at all.
	Once(ctx context.Context, pass Pass) ([]Outcome, error)
	// Activity is activity only - no digests, no swaps, no idle streaks - for
	// the tick between passes. conntrack is nil when the dump failed. It must
	// not log what a pass would already say, as it runs every few seconds.
	Activity(ctx context.Context, conntrack []detect.ConntrackEntry) ([]Activity, error)
	// List is one line per workload, for `tidewaiter list`.
	List(ctx context.Context) ([]string, error)
}

// Log is where Tidewaiter says what it is doing, one line at a time, without a
// trailing newline: the daemon, and every source, are given one. Unlike
// Portical's sources, which return warnings for the daemon to report, a
// source here logs its own - swaps, rollbacks, warnings about a label - as it
// acts, since a swap's lines must appear as it happens rather than at the end
// of a pass. A standing condition is the source's to say once, until it
// changes, not every pass.
type Log func(line string)

// MaxSwapFailures is how many consecutive swap-step failures on one target pin
// it out.
//
// Shared by every source: a single failure may be a transient hiccup (a socket
// blip, a busy Nix daemon) that says nothing about the version, but a
// deterministic one would otherwise be retried every pass forever.
const MaxSwapFailures = 3
