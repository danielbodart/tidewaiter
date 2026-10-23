package health

import (
	"context"
	"time"

	"github.com/danielbodart/gokit/clock"
)

// Timing is how long a gate waits, and how often it looks.
type Timing struct {
	// How long to wait for a verdict before trusting the update anyway.
	Timeout time.Duration
	// How often to re-evaluate the checks while waiting.
	Poll time.Duration
}

// Gate polls a set of checks until they settle, the deadline passes, or ctx is
// done.
//
// evaluate runs every check once against fresh state - a re-inspected
// container for Docker, re-read unit properties for systemd - and each time
// the verdict is Combine's: roll back on any active failure, commit once every
// applicable check has passed, otherwise keep polling. An evaluate that errors
// is a rollback, with the error as its reason. At the deadline it commits
// unless something actively failed, so a check that merely never confirmed
// cannot veto.
//
// Shared by every source, so the gate's rules are written once. ctx done -
// another arm of a race (the Docker health_status event watch) settled first,
// or the daemon is stopping - returns ctx's error and no verdict; the caller
// decides what that means. Waits are on clock, so a test drives the polls
// with a stopped one.
func Gate(ctx context.Context, clk clock.Clock, timing Timing, evaluate func(ctx context.Context) ([]Result, error)) (Verdict, error) {
	deadline := clk.Now().Add(timing.Timeout)
	for clk.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return Verdict{}, err
		}
		results, err := evaluate(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return Verdict{}, ctx.Err()
			}
			return Verdict{Reason: "health check errored: " + err.Error()}, nil
		}
		if verdict, settled := Combine(results, false); settled {
			return verdict, nil
		}
		if err := clock.Sleep(ctx, clk, timing.Poll); err != nil {
			return Verdict{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return Verdict{}, err
	}
	// The deadline: commit unless something actively failed. A last look
	// that errors has nothing to say against the update.
	results, err := evaluate(ctx)
	if err != nil {
		results = nil
	}
	verdict, _ := Combine(results, true)
	return verdict, nil
}

// UptimeSeconds is how long uptime and port-connect wait before they decide,
// given the overall health timeout in seconds.
//
// This MUST be strictly less than the health timeout, with real margin. The
// uptime check is the one that can FAIL (exited, or up but not serving) - but
// its verdict only matters if it lands *before* the gate times out, because a
// timed-out gate commits. If uptime's deadline equalled the timeout the two
// would race and the commit would usually win, so a no-backend container would
// commit instead of rolling back - a real bug this avoids.
//
// So: about two-thirds of the timeout, capped at 30s (a real service binds
// within seconds), floored at 2s so a tiny timeout still gives a just-started
// workload a moment to bind. Shared by every source, so a unit's grace is
// reckoned exactly as a container's is.
func UptimeSeconds(healthTimeoutSeconds int) int {
	return max(2, min(30, healthTimeoutSeconds*66/100))
}
