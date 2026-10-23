// Package decide is the update decision, made purely.
//
// The daemon does all the I/O - list containers, resolve the registry digest,
// sample activity, count idle streaks - and hands the result in as plain data.
// Decide then makes the call with no Docker, no registry, no netns and,
// crucially, no clock: "how long has this been idle" arrives as IdleStreak, a
// number the caller already counted, exactly as Portical's reconcile takes a
// lease as a number rather than reading the time itself.
package decide

import (
	"fmt"

	"github.com/danielbodart/tidewaiter/internal/model"
)

// Inputs is everything Decide reads about one workload, whatever is being
// updated.
//
// The systemd source judges a unit with the same rules: there Current and
// Desired are Nix store paths (the profile's target and the module's desired
// path) rather than manifest digests, but they are just as opaque - compared,
// never parsed - so nothing about the decision changes.
type Inputs struct {
	// Consecutive idle samples the policy asks for before an update.
	IdleSamples int
	// What the workload is on right now. For a container, the local image's
	// RepoDigest for its tag - the SAME kind of hash the registry returns, so
	// the two compare; never inspect's `.Image`, a config ID in a different
	// namespace that would never match. Empty when the image is not present
	// locally at all, which reads as "something to pull".
	Current model.Digest
	// What it should be on, resolved this pass.
	Desired model.Digest
	// Whether the activity gate says the workload is busy right now.
	//
	// Already the conservative combination of the chosen detectors, and
	// already fail-safe: a detector that could not tell reads as true here.
	// Decide takes this at face value.
	InUse bool
	// Consecutive passes the workload has been seen idle, counted by the
	// caller. An update is only safe once this reaches IdleSamples, so a brief
	// lull between packets does not read as an empty server.
	IdleStreak int
}

// State is the per-workload memory carried between passes.
//
// Decide only ever reads this; the caller updates it as a consequence of what
// applying the action actually did. Pinned is the one that changes a
// decision: a workload whose last update failed is pinned to the digest that
// failed, and left alone until the desired digest moves to a different one -
// chasing a broken image every pass would just fail every pass and keep
// restarting the service.
type State struct {
	Pinned model.Digest
}

// Kind is what should happen to one workload this pass.
type Kind int

const (
	Keep Kind = iota
	Update
	Defer
	Pinned
)

func (k Kind) String() string {
	switch k {
	case Keep:
		return "keep"
	case Update:
		return "update"
	case Defer:
		return "defer"
	case Pinned:
		return "pinned"
	}
	return fmt.Sprintf("Kind(%d)", int(k))
}

// Action is what should happen to one workload this pass.
//
// Deliberately scalar, not a list: unlike Portical, which reconciles a whole
// set of forwards against a whole set of mappings and derives removals from
// absence, Tidewaiter judges each workload independently and never removes
// one.
//
// Note what is NOT here: a rollback. A rollback is a consequence of a health
// check that has not happened when Decide runs - the source has to pull, stop,
// recreate and wait before it knows. So the health gate and its rollback live
// entirely in a source's swap, the same way Portical keeps the
// delete-then-add ordering of a replace out of its pure core.
type Action struct {
	Kind Kind
	// For an Update: from what (empty for nothing local yet), to what.
	From, To model.Digest
	Reason   string
}

// Words is how Decide words its reasons, so a unit's log does not talk about
// images. The rules are the same whatever is being updated; only the nouns
// change.
type Words struct {
	Pinned string
	Keep   string
	Busy   string
	// Completes "idle for N samples and ...".
	Update string
}

var ImageWords = Words{
	Pinned: "a previous update to this image failed its health check; pinned out until the tag moves on",
	Keep:   "already on the latest image",
	Busy:   "the tide is in - the container has live flows",
	Update: "a newer image is available",
}

var UnitWords = Words{
	Pinned: "a previous update to this version failed its health check; pinned out until the desired version moves on",
	Keep:   "the profile is already on the desired version",
	Busy:   "the tide is in - the unit has live flows",
	Update: "the desired version differs from the profile's",
}

// Decide what to do with one workload, purely.
//
// The order of the checks is the policy:
//
//  1. The desired digest is the one a previous update failed on -> pinned.
//     Checked FIRST, before keep: a rollback pulls the (bad) image, so the
//     local tag digest ends up equal to desired even though the container was
//     put back on the old image. Without the pin winning here, that would
//     read as "already on the latest" and the failure would be forgotten.
//  2. Already on the desired digest -> keep. Nothing to chase.
//  3. Busy, or not yet idle for long enough -> defer. Never disrupt a live
//     session to chase a version; the version will still be there next pass.
//  4. Otherwise -> update.
func Decide(in Inputs, state State, words Words) Action {
	if state.Pinned != "" && state.Pinned == in.Desired {
		return Action{Kind: Pinned, Reason: words.Pinned}
	}
	if in.Desired == in.Current {
		return Action{Kind: Keep, Reason: words.Keep}
	}
	if in.InUse {
		return Action{Kind: Defer, Reason: words.Busy}
	}
	if in.IdleStreak < in.IdleSamples {
		return Action{
			Kind:   Defer,
			Reason: fmt.Sprintf("idle %d/%d samples - waiting for the debounce window", in.IdleStreak, in.IdleSamples),
		}
	}
	return Action{
		Kind:   Update,
		From:   in.Current,
		To:     in.Desired,
		Reason: fmt.Sprintf("idle for %d samples and %s", in.IdleStreak, words.Update),
	}
}
