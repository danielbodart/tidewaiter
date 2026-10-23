package detect

import (
	"context"
	"sync"
	"time"

	"github.com/danielbodart/gokit/clock"
)

// DefaultNetioThreshold is the network bytes per second at or below which a
// container reads as idle.
const DefaultNetioThreshold = 1024

// NetBytes is a container's cumulative received plus sent bytes, from Docker's
// /stats.
type NetBytes func(ctx context.Context, id string) (uint64, error)

// Netio is a zero-privilege signal: Docker's network I/O *rate*.
//
// A rate needs two samples an interval apart, so this remembers the previous
// sample per container between passes - the same shape as the uptime check,
// which is why (like uptime) a source builds its own rather than resolving a
// shared one. The first sample for a container has nothing to compare against,
// so it reports "cannot tell", which the fail-safe reads as in use - it never
// green-lights an update on a single reading. Later samples compute bytes per
// second and give a confident verdict against threshold (0 for the default).
//
// It counts bytes, not sockets, so neither the TCP nor the UDP blind spot
// applies, but it cannot tell a health-check ping from a real client: a coarse
// supplement, offered as an explicit opt-in via tidewaiter.detector.
func Netio(bytes NetBytes, clock clock.Clock, threshold float64) Detector {
	if threshold == 0 {
		threshold = DefaultNetioThreshold
	}
	return &netio{bytes: bytes, clock: clock, threshold: threshold, previous: map[string]sample{}}
}

type sample struct {
	bytes uint64
	at    time.Time
}

type netio struct {
	bytes     NetBytes
	clock     clock.Clock
	threshold float64

	mu       sync.Mutex
	previous map[string]sample
}

func (*netio) Name() string { return "netio" }

func (n *netio) InUse(ctx context.Context, in Input) (Result, error) {
	bytes, err := n.bytes(ctx, in.ID)
	if err != nil {
		return Result{}, err
	}
	now := sample{bytes: bytes, at: n.clock.Now()}

	n.mu.Lock()
	prior, seen := n.previous[in.ID]
	n.previous[in.ID] = now
	n.mu.Unlock()

	if !seen {
		// No baseline yet - cannot compute a rate. Fail toward caution.
		return Busy, nil
	}
	seconds := now.at.Sub(prior.at).Seconds()
	// Two samples at the same instant give no usable rate; and a counter that
	// went backwards (the container was recreated) gives a meaningless one.
	if seconds <= 0 || now.bytes < prior.bytes {
		return Busy, nil
	}
	rate := float64(now.bytes-prior.bytes) / seconds
	return Result{InUse: rate > n.threshold, Confident: true}, nil
}
