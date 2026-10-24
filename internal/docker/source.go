package docker

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/danielbodart/gokit/clock"
	"github.com/danielbodart/tidewaiter/internal/decide"
	"github.com/danielbodart/tidewaiter/internal/detect"
	"github.com/danielbodart/tidewaiter/internal/health"
	"github.com/danielbodart/tidewaiter/internal/model"
	"github.com/danielbodart/tidewaiter/internal/registry"
	"github.com/danielbodart/tidewaiter/internal/source"
)

// Options are the Docker source's own settings.
type Options struct {
	// The label that opts a container in.
	Label string
	// Say what would change without changing it.
	DryRun bool
	// The stop grace when neither the label nor the container sets one.
	DefaultGraceSeconds int
	// How often the health gate looks again while it waits.
	HealthPoll time.Duration
}

// DefaultOptions are the documented defaults.
var DefaultOptions = Options{
	Label:               AutoupdateLabel,
	DefaultGraceSeconds: 10,
	HealthPoll:          2 * time.Second,
}

// Deps is what a Source is built from.
type Deps struct {
	Docker   *Client
	Registry *registry.Client
	// Credentials for a pull, keyed by registry host - the same ones the
	// registry client resolves digests with, so a private image both resolves
	// and pulls. nil is anonymous everywhere.
	Auth registry.Auth
	// A container's netns, for the tcp detector and port-bound.
	Netns detect.Netns
	// Connects for port-connect.
	TCP     health.Prober
	Options Options
	Log     source.Log
	Clock   clock.Clock
}

// Source is labelled Docker containers, kept on the digest their tag resolves
// to.
//
// Portical's reconcile reshaped for updates rather than forwards: one
// container at a time (a swap can sit minutes at its health gate, and must
// not starve the others), each one's failure its own, and the decision the
// pure decide.Decide with every input gathered first. The loop, the shared
// conntrack dump and the summary are the daemon's.
type Source struct {
	docker   *Client
	registry *registry.Client
	auth     registry.Auth
	netns    detect.Netns
	tcp      health.Prober
	options  Options
	log      source.Log
	clock    clock.Clock

	// Built once and kept: it remembers each container's previous byte
	// sample to work out a rate across passes.
	netio detect.Detector

	memory map[string]memory
	// What was last said of each container's labels, so a bad one is said
	// once rather than every pass.
	said map[string]string

	// The target digest of the background pull per container, by name.
	//
	// Not part of memory, which the decision reads: a warmed cache changes
	// nothing about the *decision*, only how fast the swap runs. The digest is
	// the dedupe key - one pull per target, relaunched only when the tag moves
	// on, or when a pull failed and cleared its entry. Guarded, as the pulls
	// finish on their own goroutines.
	mu         sync.Mutex
	prefetches map[string]model.Digest
	pulling    sync.WaitGroup
}

// memory is what is carried about one container from pass to pass.
type memory struct {
	idleStreak int
	// The digest a failed update pinned out, until the tag moves on.
	pinned model.Digest
	// The image the container ran before the last update.
	previous *image
	// Digests kept beyond the current one, newest first, for keep-images.
	kept []model.Digest
	// Once the "adopting as baseline" note has been said.
	adopted bool
	// Consecutive swap-step failures against failing, for back-off.
	//
	// A single swap-step failure is not pinned: it may be a transient Docker
	// hiccup - a socket blip, momentary disk pressure - that says nothing about
	// the image. But a deterministic one (a genuinely broken image) would
	// otherwise retry every pass forever. After source.MaxSwapFailures in a
	// row on the same digest it is pinned out, as a health failure pins it,
	// and a new digest clears the count.
	swapFailures int
	failing      model.Digest
}

type image struct {
	ref    string
	digest model.Digest
}

// NewSource is a Docker source over deps.
func NewSource(deps Deps) *Source {
	if deps.Log == nil {
		deps.Log = func(string) {}
	}
	// A background pull says how it went on its own goroutine, while a pass may
	// be speaking: their lines are taken one at a time.
	var speaking sync.Mutex
	log := deps.Log
	deps.Log = func(line string) {
		speaking.Lock()
		defer speaking.Unlock()
		log(line)
	}
	if deps.Clock == nil {
		deps.Clock = clock.System{}
	}
	if deps.Auth == nil {
		deps.Auth = registry.Anonymous
	}
	if deps.TCP == nil {
		deps.TCP = health.Dialer{}
	}
	s := &Source{
		docker: deps.Docker, registry: deps.Registry, auth: deps.Auth, netns: deps.Netns, tcp: deps.TCP,
		options: deps.Options, log: deps.Log, clock: deps.Clock,
		memory: map[string]memory{}, said: map[string]string{}, prefetches: map[string]model.Digest{},
	}
	s.netio = detect.Netio(deps.Docker.NetBytes, deps.Clock, 0)
	return s
}

func (*Source) Name() source.Name { return source.Docker }

// Once brings every opted-in container into line with its tag, once.
//
// A listing that fails is the pass's error - a pass that cannot see the
// containers must not proceed as if there were none.
func (s *Source) Once(ctx context.Context, pass source.Pass) ([]source.Outcome, error) {
	containers, err := s.docker.Containers(ctx, s.options.Label)
	if err != nil {
		return nil, err
	}
	outcomes := make([]source.Outcome, 0, len(containers))
	for _, container := range containers {
		outcomes = append(outcomes, s.reconcile(ctx, container, pass))
	}
	return outcomes, nil
}

// Activity is whether each opted-in container is busy now, for the tick
// between passes.
//
// The same detectors and fail-safe as a pass, but nothing is resolved,
// decided or remembered - in particular no idle streak moves, so the faster
// tick can never hurry an update along - and nothing said that a pass says.
func (s *Source) Activity(ctx context.Context, conntrack []detect.ConntrackEntry) ([]source.Activity, error) {
	containers, err := s.docker.Containers(ctx, s.options.Label)
	if err != nil {
		return nil, err
	}
	var activities []source.Activity
	for _, container := range containers {
		policy, _ := ParseLabels(s.options.Label, container.Spec.Labels, container.Spec.Published())
		if policy == nil {
			continue
		}
		result := s.sample(ctx, container, policy, conntrack, false)
		activities = append(activities, source.Activity{Workload: container.Spec.Name, Busy: result.InUse})
	}
	return activities, nil
}

// List is one line per opted-in container: its image, whether an update
// waits, and its policy.
func (s *Source) List(ctx context.Context) ([]string, error) {
	containers, err := s.docker.Containers(ctx, s.options.Label)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, container := range containers {
		spec := container.Spec
		policy, _ := ParseLabels(s.options.Label, spec.Labels, spec.Published())
		if policy == nil {
			// Has the label key but not a policy - shown so a typo is visible.
			lines = append(lines, fmt.Sprintf("%s  %s  [not a known autoupdate policy]", spec.Name, spec.Image))
			continue
		}
		// Resolved as a pass would for this policy, and compared like with
		// like: the running image's own manifest digest, never inspect's
		// config id.
		desired, err := s.desired(ctx, spec.Image, policy)
		state := "update available"
		switch {
		case policy.Autoupdate == Local && (err != nil || desired == ""):
			state = "no local image"
		case err != nil:
			state = "registry unreachable"
		default:
			if current, err := s.docker.ImageDigest(ctx, container.ImageID); err == nil && current == desired {
				state = "up to date"
			}
		}
		lines = append(lines, fmt.Sprintf("%s  %s  [%s]  autoupdate=%s detector=%s health=%s idle-samples=%d",
			spec.Name, spec.Image, state, policy.Autoupdate, policy.Detector, strings.Join(policy.Health, ","), policy.IdleSamples))
	}
	return lines, nil
}

// desired is what the tag should resolve to now, per the policy: the
// registry's manifest digest for it, or for local the stored image's, with
// no registry contact - "" when the tag is not here at all.
func (s *Source) desired(ctx context.Context, ref string, policy *Policy) (model.Digest, error) {
	if policy.Autoupdate == Local {
		return s.docker.ImageDigest(ctx, ref)
	}
	return s.registry.Digest(ctx, ref)
}

// reconcile decides and applies for one container.
func (s *Source) reconcile(ctx context.Context, container model.RunningContainer, pass source.Pass) source.Outcome {
	name := container.Spec.Name
	keep := func(reason string) source.Outcome {
		return source.Outcome{Workload: name, Action: decide.Action{Kind: decide.Keep, Reason: reason}}
	}
	policy, warnings := ParseLabels(s.options.Label, container.Spec.Labels, container.Spec.Published())
	s.note(name, warnings)

	// Not opted in - the listing filters on the label key, so here that means
	// a value we do not know, which the warning has said. Left completely
	// alone, and nothing to wait for.
	if policy == nil {
		return source.Outcome{Workload: name, Action: decide.Action{Kind: decide.Keep, Reason: "not opted in"}, Busy: source.Sampled(false), Settled: true}
	}

	previous := s.memory[name]

	// Either source failing leaves the container alone this pass - never
	// updated on a guess.
	desired, err := s.desired(ctx, container.Spec.Image, policy)
	if err != nil {
		where := "registry"
		if policy.Autoupdate == Local {
			where = "local image"
		}
		s.log(fmt.Sprintf("%s: could not resolve %s digest (%s); leaving it alone", name, where, err))
		return keep(where + " unreachable")
	}
	if desired == "" {
		return keep("no local image for the tag to update to")
	}

	// The manifest digest the container is actually running: the RepoDigest
	// of the very image it was created from, looked up by that image's id,
	// NOT by the tag. Inspect's .Image is a config id, a different hash that
	// would never match a registry answer; and the tag drifts the moment a
	// pull moves it - after a failed swap or a rollback it points at the new
	// (bad) image while the container still runs the old one. "" means that
	// image has no registry digest here (built, not pulled), which reads as
	// something to pull.
	//
	// The same lookup gives the running image's defaults, which a swap takes
	// back out of the container's config so the new image's apply instead -
	// read here, before anything is decided, so a lookup that fails has
	// touched nothing.
	running, found, err := s.docker.Image(ctx, container.ImageID)
	if err != nil {
		s.log(fmt.Sprintf("%s: could not read the running image digest (%s); leaving it alone", name, err))
		return keep("running image digest unreadable")
	}
	current := running.Digest
	var defaults *ImageDefaults
	if found {
		defaults = &running.Defaults
	}

	// A pin only holds until the tag moves on.
	if previous.pinned != "" && previous.pinned != desired {
		previous.pinned = ""
	}
	// First sight, on neither the desired digest nor one we recorded: adopt
	// it as the baseline rather than assume anything about a swap we have no
	// memory of.
	if !previous.adopted && current != desired && (previous.previous == nil || previous.previous.digest != current) {
		s.log(name + ": adopting the running image as the current baseline (no rollback record from a prior run)")
		previous.adopted = true
	}

	s.prefetch(ctx, container, policy, desired, current, previous.pinned)

	activity := s.sample(ctx, container, policy, pass.Conntrack, true)
	idleStreak := 0
	if !activity.InUse {
		idleStreak = previous.idleStreak + 1
	}
	action := decide.Decide(decide.Inputs{
		IdleSamples: policy.IdleSamples,
		Current:     current,
		Desired:     desired,
		InUse:       activity.InUse,
		IdleStreak:  idleStreak,
	}, decide.State{Pinned: previous.pinned}, decide.ImageWords)

	// Held before the swap starts, so an automatic reboot cannot land mid-way.
	if action.Kind == decide.Update && !s.options.DryRun && pass.Hold != nil {
		pass.Hold(name)
	}
	s.memory[name] = s.apply(ctx, action, container, defaults, policy, previous, idleStreak, desired, current)
	return source.Outcome{
		Workload: name,
		Action:   action,
		Busy:     source.Sampled(activity.InUse),
		// A container just recreated is settled again only once a pass says so.
		Settled: !activity.InUse && idleStreak >= policy.IdleSamples && action.Kind != decide.Update,
	}
}

// note says a container's label warnings when they differ from what was last
// said of it: a standing misconfiguration is said once, not every pass.
func (s *Source) note(name string, warnings []string) {
	said := strings.Join(warnings, "\n")
	if s.said[name] == said {
		return
	}
	s.said[name] = said
	for _, warning := range warnings {
		s.log(name + ": " + warning)
	}
}

// sample is whether the container is busy, fail-safe.
//
// A detector that errs - /proc gone because the container just died, a
// /stats read refused - must never abort the pass or read as idle: it is
// "could not tell", which is busy, so uncertainty always defers rather than
// disrupts. So is a conntrack table that could not be read, for the detector
// that needs it, and a ports override that selected nothing.
func (s *Source) sample(ctx context.Context, container model.RunningContainer, policy *Policy, conntrack []detect.ConntrackEntry, say bool) detect.Result {
	ports := container.Spec.Published()
	if policy.PortsOverride {
		if len(policy.Ports) == 0 {
			return detect.Busy
		}
		ports = policy.Ports
	}
	detector := s.netio
	if policy.Detector != "netio" {
		var known bool
		if detector, known = detect.ByName(policy.Detector); !known {
			return detect.Busy
		}
	}
	if detector == detect.Conntrack && conntrack == nil {
		return detect.Busy
	}
	result, err := detector.InUse(ctx, detect.Input{
		ID:        container.ID,
		PID:       container.PID,
		Published: ports,
		Conntrack: conntrack,
		Netns:     s.netns,
	})
	if err != nil {
		if say {
			s.log(fmt.Sprintf("%s: activity detector '%s' failed (%s); assuming busy", container.Spec.Name, policy.Detector, err))
		}
		return detect.Busy
	}
	return detect.Combine(result)
}

func (s *Source) apply(ctx context.Context, action decide.Action, container model.RunningContainer, defaults *ImageDefaults, policy *Policy, previous memory, idleStreak int, desired, current model.Digest) memory {
	name := container.Spec.Name
	base := previous
	base.idleStreak = idleStreak
	switch action.Kind {
	case decide.Defer:
		s.log(name + ": " + action.Reason)
	case decide.Pinned:
		base.pinned = desired
	case decide.Update:
		s.log(name + ": updating - " + action.Reason)
		if s.options.DryRun {
			s.log(name + ": dry run, not touching it")
			return base
		}
		return s.update(ctx, container, defaults, policy, base, desired, current)
	}
	return base
}

// prefetch starts a background pull of a newer image, so it is already here
// by the time the container goes idle enough to swap - the swap is then a
// fast local recreate, not a multi-minute download inside the very idle
// window we were waiting for.
//
// Detached on purpose: the pass moves straight on, even while the container
// is busy. Only for the registry policy (local never touches the network),
// only when there is something newer, never for a pinned-out (bad) digest,
// never in a dry run. A failed pull is purely speculative - it never pins; it
// clears its record so the next pass retries, and the swap does its own pull
// with full error handling regardless.
func (s *Source) prefetch(ctx context.Context, container model.RunningContainer, policy *Policy, desired, current, pinned model.Digest) {
	if s.options.DryRun || policy.Autoupdate != Registry || desired == current || desired == pinned {
		return
	}
	name, ref := container.Spec.Name, container.Spec.Image
	s.mu.Lock()
	if s.prefetches[name] == desired {
		s.mu.Unlock()
		return
	}
	s.prefetches[name] = desired
	s.mu.Unlock()

	s.log(fmt.Sprintf("%s: prefetching %s in the background so the swap is ready when the tide goes out", name, ref))
	s.pulling.Go(func() {
		// Not the pass's to cancel: the pull outlives it, bounded by
		// PullTimeout alone.
		if err := s.docker.Pull(context.WithoutCancel(ctx), ref, s.authFor(ref)); err != nil {
			s.mu.Lock()
			if s.prefetches[name] == desired {
				delete(s.prefetches, name)
			}
			s.mu.Unlock()
			s.log(fmt.Sprintf("%s: prefetch of %s failed (%s); will retry", name, ref, err))
		}
	})
}

// Prefetched waits for every background pull started so far to finish.
func (s *Source) Prefetched() { s.pulling.Wait() }

// authFor is the X-Registry-Auth for an image's registry, or "".
func (s *Source) authFor(ref string) string {
	return s.auth(registry.ParseRef(ref).Registry)
}

// progress is how far a swap got, so a failure undoes exactly that.
//
// parked is the load-bearing one: true only once the old container has been
// renamed out of the way, which is what tells the undo whether the original
// still wears its own name (leave it be) or has been parked (restore it).
type progress struct {
	stopped, parked, created, started bool
}

// update is the swap: pull, stop, rename-not-delete recreate, health gate,
// commit or roll back. Every failure is logged and leaves the container
// running whatever it was - one bad update must not take the daemon down.
//
// Two things a naive version gets wrong, both handled here:
//
//   - A swap step (not just the health check) can fail, and the rollback that
//     is safe after a *complete* swap is dangerous part-way through one. So
//     the swap records how far it got, and the undo is derived from that -
//     never force-removing the original before it has been safely parked.
//   - A transient Docker error must not pin the tag. Pinning means "this
//     image is bad", and a bounced API call says nothing about the image.
//     Only a genuine health failure of a stood-up new container pins (or the
//     same swap failing source.MaxSwapFailures times running).
//
// defaults is the running image's, nil when that image is no longer here.
func (s *Source) update(ctx context.Context, container model.RunningContainer, defaults *ImageDefaults, policy *Policy, base memory, desired, current model.Digest) memory {
	spec := container.Spec
	name := spec.Name
	if reason := Unrecreatable(spec); reason != "" {
		s.log(name + ": " + reason)
		return base
	}
	// Without the old image's defaults there is no telling the container's own
	// settings from the ones it inherited, and copying them all would pin the
	// old image's CMD, ENV and HEALTHCHECK onto the new one - silently, as a
	// swap that looks like it worked. Docker keeps an image while a container
	// uses it, so this is an anomaly worth saying, not a case to guess at; it
	// is refused like an anonymous volume, and pins nothing.
	if defaults == nil {
		s.log(fmt.Sprintf("%s: the image it was created from (%s) is no longer here, so which of its settings "+
			"are its own and which that image's cannot be told; deferring rather than carry the old image's "+
			"defaults onto the new one", name, container.ImageID))
		return base
	}
	// The reference is fragile: if that other container was itself
	// recreated, `container:<id>` names an id that no longer exists. Said at
	// the moment of a swap rather than every idle pass, and not a refusal -
	// it works as long as the other container is stable.
	if strings.HasPrefix(spec.NetworkMode, "container:") {
		s.log(fmt.Sprintf("%s: network mode is '%s', which shares another container's netns; "+
			"recreating it verbatim - if that container was replaced the reference may be stale", name, spec.NetworkMode))
	}

	grace := s.options.DefaultGraceSeconds
	switch {
	case policy.GraceSeconds != nil:
		grace = *policy.GraceSeconds
	case spec.StopTimeout != nil:
		grace = *spec.StopTimeout
	}
	// The digest being left, for rollback and keep-images. When the image was
	// somehow not here there is nothing to go back to, and a health failure
	// cannot pretend to restore what never existed.
	var leaving *image
	if current != "" {
		leaving = &image{ref: spec.Image, digest: current}
	}
	// The tag is re-pulled as it is: the ref does not change, only what it
	// resolves to - chasing the tag *is* the update. The new spec is the old
	// one less its old image's defaults, so the new image's take their place.
	plan := PlanSwap(spec, WithoutImageDefaults(spec, *defaults), grace)

	// Only the registry policy pulls: under local the newer image is already
	// here (that is the point of it), and pulling would defeat it, or fail
	// outright on an air-gapped or build-on-host box.
	if policy.Autoupdate == Registry {
		if err := s.docker.Pull(ctx, spec.Image, s.authFor(spec.Image)); err != nil {
			s.log(fmt.Sprintf("%s: FAILED to pull %s (%s); leaving it running", name, spec.Image, err))
			return base
		}
	}

	done, err := s.run(ctx, plan.Swap)
	if err != nil {
		s.log(fmt.Sprintf("%s: FAILED during swap (%s); undoing what was done", name, err))
		s.undo(ctx, name, done)
		failures := 1
		if base.failing == desired {
			failures = base.swapFailures + 1
		}
		next := base
		next.previous, next.swapFailures, next.failing = leaving, failures, desired
		if failures >= source.MaxSwapFailures {
			s.log(fmt.Sprintf("%s: swap has failed %d times running for %s; pinning it out until the tag moves on", name, failures, desired))
			next.pinned = desired
		}
		return next
	}

	verdict := s.gate(ctx, name, policy)
	if verdict.Healthy {
		if err := s.steps(ctx, plan.Commit); err != nil {
			s.log(fmt.Sprintf("%s: updated, but cleaning up the old container failed (%s)", name, err))
		}
		kept := base.kept
		if leaving != nil {
			kept = s.prune(ctx, name, leaving, base.kept, policy.KeepImages)
		}
		s.log(fmt.Sprintf("%s: updated to %s and it is healthy", name, desired))
		return memory{idleStreak: base.idleStreak, kept: kept, previous: leaving, adopted: true}
	}

	// A genuinely unhealthy new image after a complete swap: the one case
	// that both rolls fully back and pins the tag out.
	s.log(fmt.Sprintf("%s: new image is unhealthy (%s); rolling back", name, verdict.Reason))
	s.undo(ctx, name, done)
	s.log(name + ": rolled back to the previous image and pinned it out until the tag moves on")
	next := base
	next.pinned, next.previous = desired, leaving
	return next
}

// run carries out the swap's steps, remembering how far it got.
func (s *Source) run(ctx context.Context, steps []Step) (progress, error) {
	var done progress
	for _, step := range steps {
		if err := s.step(ctx, step); err != nil {
			return done, err
		}
		switch step.Op {
		case Stop:
			done.stopped = true
		case Rename:
			done.parked = true
		case Create:
			done.created = true
		case Start:
			done.started = true
		}
	}
	return done, nil
}

// undo takes back a swap that got as far as done, best effort.
//
// Never force-removes the original before it has been parked - the bug a
// blind rollback had. The new container, if one was created, wears the
// original name, so it goes first to free the name; then the parked original
// gets its name back and starts, or, never parked, simply starts again.
func (s *Source) undo(ctx context.Context, name string, done progress) {
	// Finished even if the pass is being given up on: a half-swapped
	// container left down is the one outcome worse than either.
	ctx = context.WithoutCancel(ctx)
	var steps []Step
	if done.created {
		steps = append(steps, Step{Op: Remove, Container: name, Force: true})
	}
	switch {
	case done.parked:
		steps = append(steps, Step{Op: Rename, Container: RollbackName(name), To: name}, Step{Op: Start, Container: name})
	case done.stopped:
		steps = append(steps, Step{Op: Start, Container: name})
	}
	if err := s.steps(ctx, steps); err != nil {
		s.log(fmt.Sprintf("%s: rollback itself failed (%s); manual attention needed", name, err))
	}
}

func (s *Source) steps(ctx context.Context, steps []Step) error {
	for _, step := range steps {
		if err := s.step(ctx, step); err != nil {
			return err
		}
	}
	return nil
}

func (s *Source) step(ctx context.Context, step Step) error {
	switch step.Op {
	case Stop:
		return s.docker.Stop(ctx, step.Container, step.Grace)
	case Rename:
		return s.docker.Rename(ctx, step.Container, step.To)
	case Create:
		_, err := s.docker.Create(ctx, step.Spec)
		return err
	case Connect:
		return s.docker.ConnectNetwork(ctx, step.Container, step.Network)
	case Start:
		return s.docker.Start(ctx, step.Container)
	case Remove:
		return s.docker.Remove(ctx, step.Container, step.Force)
	}
	return fmt.Errorf("unknown swap step %q", step.Op)
}

// prune removes images beyond keep-images, newest kept first.
//
// The just-superseded image and the keepImages-1 before it stay, for an
// immediate rollback by hand; anything older goes. Each is removed by its
// repository digest (`app@sha256:...`): a manifest digest alone is not an
// image id, and Docker would not find it.
func (s *Source) prune(ctx context.Context, name string, superseded *image, kept []model.Digest, keepImages int) []model.Digest {
	history := slices.Concat([]model.Digest{superseded.digest}, kept)
	keepImages = min(max(0, keepImages), len(history))
	for _, digest := range history[keepImages:] {
		reference := repository(superseded.ref) + "@" + string(digest)
		if err := s.docker.RemoveImage(ctx, reference); err != nil {
			s.log(fmt.Sprintf("%s: could not prune image %s (%s)", name, digest, err))
		}
	}
	return slices.Clone(history[:keepImages])
}

// repository is a ref without its tag or digest: `ghcr.io:443/me/app:1` is
// `ghcr.io:443/me/app`.
func repository(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	if colon := strings.LastIndex(ref, ":"); colon > strings.LastIndex(ref, "/") {
		return ref[:colon]
	}
	return ref
}

// gate waits for the new container to prove healthy, up to the policy's
// timeout.
//
// The checks are built once, so the stateful uptime and port-connect keep
// their start time across every evaluation. Two arms race:
//   - the poll, always: every check re-evaluated each interval against a
//     fresh inspect, and combined;
//   - when `docker` is one of the checks, a watch of Docker's health_status
//     events, which re-runs the same combination the instant Docker's own
//     probe flips (with the docker check taken from the event) - so a
//     healthy or unhealthy event settles the gate without waiting out a poll.
//
// The first verdict wins; the other arm is cancelled and waited for, so
// nothing outlives the gate.
func (s *Source) gate(ctx context.Context, name string, policy *Policy) health.Verdict {
	checks := s.checksFor(policy)
	drivers := s.networkDrivers()
	// One evaluation at a time: the checks are stateful, and the two arms
	// would otherwise evaluate them at once.
	var evaluating sync.Mutex
	evaluate := func(ctx context.Context, override *health.Result) ([]health.Result, error) {
		evaluating.Lock()
		defer evaluating.Unlock()
		fresh, err := s.docker.Inspect(ctx, name)
		if err != nil {
			return nil, err
		}
		input := health.Input{
			Subject:       health.ContainerSubject(fresh),
			Ports:         fresh.Spec.Published(),
			Bound:         health.NetnsBoundPorts(s.netns, fresh.PID),
			TCP:           s.tcp,
			NetworkDriver: drivers,
		}
		if policy.PortsOverride {
			input.Ports = policy.Ports
		}
		results := make([]health.Result, 0, len(checks))
		for _, check := range checks {
			if check.Name() == Check.Name() && override != nil {
				// The event already has Docker's verdict; a re-read inspect may
				// not have caught up with it yet.
				results = append(results, *override)
				continue
			}
			results = append(results, check.Evaluate(ctx, input))
		}
		return results, nil
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	verdicts := make(chan health.Verdict, 2)
	var arms sync.WaitGroup
	arms.Go(func() {
		timing := health.Timing{Timeout: time.Duration(policy.HealthTimeoutSeconds) * time.Second, Poll: s.options.HealthPoll}
		verdict, err := health.Gate(ctx, s.clock, timing, func(ctx context.Context) ([]health.Result, error) {
			return evaluate(ctx, nil)
		})
		if err == nil {
			verdicts <- verdict
		}
	})
	if slices.Contains(policy.Health, Check.Name()) {
		arms.Go(func() {
			if verdict, settled := s.watch(ctx, name, evaluate); settled {
				verdicts <- verdict
			}
		})
	}
	finished := make(chan struct{})
	go func() {
		arms.Wait()
		close(finished)
	}()

	select {
	case verdict := <-verdicts:
		cancel()
		<-finished
		return verdict
	case <-finished:
		select {
		case verdict := <-verdicts:
			return verdict
		default:
			// Neither arm could finish: the pass itself was given up on.
			return health.Verdict{Reason: "the health gate was cancelled"}
		}
	}
}

// watch is the gate's event arm: settled on the first health_status that,
// with the other checks, gives a verdict. A stream that drops is opened
// again; until ctx is done, which is how the poll winning ends it.
func (s *Source) watch(ctx context.Context, name string, evaluate func(context.Context, *health.Result) ([]health.Result, error)) (health.Verdict, bool) {
	var verdict health.Verdict
	settled := false
	for ctx.Err() == nil {
		s.docker.Events(ctx, s.options.Label, func(event Event) bool {
			if event.Container != name || !strings.HasPrefix(event.Action, "health_status") {
				return false
			}
			status := model.HealthStatus(event.Status)
			if status != model.Healthy && status != model.Unhealthy {
				return false
			}
			override := Verdict(status)
			results, err := evaluate(ctx, &override)
			if err != nil {
				return false
			}
			verdict, settled = health.Combine(results, false)
			return settled
		})
		if settled {
			return verdict, true
		}
		// Dropped, or never opened - why changes nothing: the
		// poll carries the wait meanwhile, and a moment later we look again.
		if clock.Sleep(ctx, s.clock, time.Second) != nil {
			break
		}
	}
	return health.Verdict{}, false
}

// checksFor is the gate's checks, from tidewaiter.health. uptime and
// port-connect are built afresh, as each remembers when it first saw the
// container; both wait the same grace before they decide.
func (s *Source) checksFor(policy *Policy) []health.Check {
	seconds := health.UptimeSeconds(policy.HealthTimeoutSeconds)
	var checks []health.Check
	for _, name := range policy.Health {
		switch name {
		case "docker":
			checks = append(checks, Check)
		case "port-bound":
			checks = append(checks, health.PortBound)
		case "uptime":
			checks = append(checks, health.Uptime(seconds, s.clock))
		case "port-connect":
			checks = append(checks, health.PortConnect(seconds, s.clock))
		}
	}
	return checks
}

// networkDrivers is a network-driver lookup for one gate.
//
// Memoised, so port-connect (evaluated every poll, on two arms) asks
// /networks once per network per swap, not once per poll; and built afresh
// each swap, so it is never stale. Failure-tolerant: a lookup that errs is
// "" - not a driver we can route to - so a /networks hiccup never surfaces
// as a health failure and rolls back a healthy update.
func (s *Source) networkDrivers() func(ctx context.Context, network string) string {
	var mu sync.Mutex
	known := map[string]string{}
	return func(ctx context.Context, network string) string {
		mu.Lock()
		defer mu.Unlock()
		if driver, ok := known[network]; ok {
			return driver
		}
		driver, _ := s.docker.NetworkDriver(ctx, network)
		known[network] = driver
		return driver
	}
}
