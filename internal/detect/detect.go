// Package detect tells whether a workload is in use: the Detector seam, the
// detectors themselves, and the conservative rule that combines them.
//
// Everything here is pure over its inputs - the conntrack table arrives as
// typed entries, a netns's sockets as /proc text through Netns - so the
// Docker and systemd sources share one set of detectors, and the kernel-facing
// readers that feed them (ctnetlink, /proc) live elsewhere, behind
// ConntrackDumper and Netns.
package detect

import (
	"context"

	"github.com/danielbodart/tidewaiter/internal/model"
)

// Result is what a detector reports about one workload.
//
// Sessions is only meaningful when Counted: a detector can sense presence
// without counting it - a UDP server shares one socket across all clients, so
// conntrack can say "busy" without saying "how many". Confident lets the
// caller tell a real "idle" from a "could not tell", which under the
// fail-safe rule become very different outcomes.
type Result struct {
	InUse     bool
	Sessions  int
	Counted   bool
	Confident bool
}

// Busy is "could not tell", which the fail-safe always reads as in use.
var Busy = Result{InUse: true}

// Input is what a detector is given about one workload.
type Input struct {
	// The container id or unit name, for detectors that remember per workload.
	ID string
	// The workload's main process, from whose /proc its netns is read.
	PID       int
	Published []model.PublishedPort
	// The host conntrack table for this pass, dumped once and shared.
	Conntrack []ConntrackEntry
	Netns     Netns
}

// Detector is a way of telling whether a workload is in use, per PLAN.md's
// Detector idea.
//
// Kept pluggable, and combined conservatively (in use if any says so), so a
// future protocol-aware detector can be added without touching a source. An
// error is "could not tell": the caller logs it and reads it as Busy.
type Detector interface {
	Name() string
	InUse(ctx context.Context, in Input) (Result, error)
}

// ByName resolves the stateless detectors: conntrack, tcp and none.
//
// netio is absent on purpose: it remembers the previous byte sample and needs
// Docker's stats and a clock, so a source builds its own with Netio, exactly
// as it builds the uptime check per swap.
func ByName(name string) (Detector, bool) {
	switch name {
	case "conntrack":
		return Conntrack, true
	case "tcp":
		return TCP, true
	case "none":
		return None, true
	}
	return nil, false
}

// Combine several detector results conservatively: in use if ANY says so.
//
// Pure, taking results rather than detectors, so the composition rule is
// testable on its own. Confident only when a confident detector actually
// contributed to the verdict - so a lone "cannot tell" stays unconfident, and
// the fail-safe still applies.
func Combine(results ...Result) Result {
	var combined Result
	for _, result := range results {
		combined.InUse = combined.InUse || result.InUse
	}
	for _, result := range results {
		if result.InUse == combined.InUse && result.Confident {
			combined.Confident = true
		}
		if result.Counted {
			combined.Counted = true
			combined.Sessions += result.Sessions
		}
	}
	return combined
}

// None opts out of activity detection entirely - always idle, unconfidently.
var None Detector = none{}

type none struct{}

func (none) Name() string { return "none" }

func (none) InUse(context.Context, Input) (Result, error) { return Result{}, nil }
