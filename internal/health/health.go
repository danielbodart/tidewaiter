// Package health is the combined health gate a swap must pass before it is
// trusted: the checks every source shares, the rule that combines them, and
// the poll that waits for a verdict.
//
// The Docker source adds its own `docker` check and the systemd source its
// `systemd` one; both are Checks like these, combined by the same rule.
package health

import (
	"context"
	"fmt"

	"github.com/danielbodart/tidewaiter/internal/model"
)

// Outcome is what one check concluded this poll.
//
// The whole design leans on the pass/inconclusive/skip distinction: only a
// Fail rolls an update back, because false-unhealthy (our probe wrongly
// vetoing a good update) is the worse error - a genuinely broken image that
// merely goes quiet is caught next cycle by the activity gate or a crash loop.
type Outcome int

const (
	// Inconclusive is not confirmed yet; keep polling (still starting,
	// nothing bound yet, uptime not elapsed). Crucially NOT a failure - a slow
	// or flaky probe must never veto a good update. The zero Outcome, so a
	// check that says nothing in particular holds the gate open.
	Inconclusive Outcome = iota
	// Pass is affirmatively confirmed: Docker says healthy, a port accepted.
	Pass
	// Fail is actively broken: Docker says unhealthy, the container exited.
	Fail
	// Skip is a check that cannot apply here - docker with no HEALTHCHECK,
	// port-connect with no TCP ports. It contributes nothing to the verdict.
	Skip
)

func (o Outcome) String() string {
	switch o {
	case Inconclusive:
		return "inconclusive"
	case Pass:
		return "pass"
	case Fail:
		return "fail"
	case Skip:
		return "skip"
	}
	return fmt.Sprintf("Outcome(%d)", int(o))
}

// Result is one check's outcome, with the reason for a Fail.
type Result struct {
	Outcome Outcome
	Reason  string
}

var (
	Passed  = Result{Outcome: Pass}
	Waiting = Result{Outcome: Inconclusive}
	Skipped = Result{Outcome: Skip}
)

// Failed is a Fail, saying why - which becomes the rollback's reason.
func Failed(reason string) Result { return Result{Outcome: Fail, Reason: reason} }

// Verdict is the combined gate's: commit when Healthy, otherwise roll back
// for Reason.
type Verdict struct {
	Healthy bool
	Reason  string
}

// Subject is what the checks read about the thing being gated.
//
// A container gives it as ContainerSubject does. A systemd unit is Running
// while its ActiveState is active, has no Docker health (NoHealth, so docker
// skips) and is on the host's network (NetworkMode "host", no networks), which
// makes port-connect probe 127.0.0.1 at the unit's own ports as a truthful
// endpoint.
type Subject struct {
	Running     bool
	Health      model.HealthStatus
	Networks    []model.NetworkAttachment
	NetworkMode string
}

// ContainerSubject is a running container as the checks see it.
func ContainerSubject(container model.RunningContainer) Subject {
	return Subject{
		Running:     container.Running,
		Health:      container.Health,
		Networks:    container.Spec.Networks,
		NetworkMode: container.Spec.NetworkMode,
	}
}

// Input is everything a check is evaluated against, fresh each poll.
type Input struct {
	Subject Subject
	// The ports in scope: the tidewaiter.ports override, or what is published.
	Ports []model.PublishedPort
	Bound BoundPorts
	TCP   Prober
	// A network's driver (bridge, macvlan, overlay, host, ...) by name, or ""
	// for one that is gone or cannot be resolved, which reads as not
	// reachable - an IP that cannot be classified is never probed to fail.
	// Injected like Bound and TCP so tests answer without Docker; the Docker
	// source memoises its /networks lookups for the length of one gate. Nil
	// for a subject with no networks.
	NetworkDriver func(ctx context.Context, network string) string
}

// Check is one health check: a name plus an evaluation to a Result.
//
// Evaluated fresh each poll against fresh state. Stateless except uptime and
// port-connect, which are built per swap so they can remember when they first
// saw the subject - build a new set of checks for every gate.
type Check interface {
	Name() string
	Evaluate(ctx context.Context, in Input) Result
}

// Combine the checks' results into a verdict, or false for "keep polling".
//
// The rule, tuned so a flaky probe cannot veto a good update:
//   - ANY check failed: roll back, immediately.
//   - not timed out yet: every applicable check passed (at least one pass,
//     none inconclusive) commits; otherwise keep polling.
//   - timed out: commit, UNLESS something failed. A check still inconclusive
//     at the deadline does not veto - "we could not fully confirm in time"
//     reads as "trust the update", because false-unhealthy is the worse error
//     and a genuinely broken image is caught next cycle.
//
// A gate where every check skipped settles to commit - but in practice
// uptime always applies and passes before the timeout, so that is an edge
// case only.
func Combine(results []Result, timedOut bool) (Verdict, bool) {
	for _, result := range results {
		if result.Outcome == Fail {
			reason := result.Reason
			if reason == "" {
				reason = "a health check failed"
			}
			return Verdict{Reason: reason}, true
		}
	}
	if timedOut {
		return Verdict{Healthy: true}, true
	}
	for _, result := range results {
		if result.Outcome == Inconclusive {
			return Verdict{}, false
		}
	}
	// Every applicable check passed, or every one skipped: nothing left to
	// wait for.
	return Verdict{Healthy: true}, true
}
