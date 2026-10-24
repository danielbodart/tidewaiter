package docker

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/danielbodart/gokit/await"
	"github.com/danielbodart/gokit/clock"
	"github.com/danielbodart/gokit/httpx"
	"github.com/danielbodart/gokit/logx/logtest"
	"github.com/danielbodart/tidewaiter/internal/daemon"
	"github.com/danielbodart/tidewaiter/internal/decide"
	"github.com/danielbodart/tidewaiter/internal/detect"
	"github.com/danielbodart/tidewaiter/internal/fakes"
	"github.com/danielbodart/tidewaiter/internal/model"
	"github.com/danielbodart/tidewaiter/internal/registry"
	"github.com/danielbodart/tidewaiter/internal/source"
)

// world is the whole daemon over the Docker source, wired to a Docker, a
// registry, a conntrack table and netns that exist only in memory: the real
// client, the real reconcile, decide and swap, with only the far ends
// replaced.
type world struct {
	docker    *fakeDocker
	registry  *fakeRegistry
	conntrack *fakes.Conntrack
	netns     *fakes.Netns
	tcp       *fakes.Prober
	log       *logtest.Logger
	clock     clock.Clock
	source    *Source
	daemon    *daemon.Daemon
}

type setting func(*Deps)

func dryRun(deps *Deps)                   { deps.Options.DryRun = true }
func pollEvery(d time.Duration) setting   { return func(deps *Deps) { deps.Options.HealthPoll = d } }
func withClock(c clock.Clock) setting     { return func(deps *Deps) { deps.Clock = c } }
func withAuth(auth registry.Auth) setting { return func(deps *Deps) { deps.Auth = auth } }

func newWorld(t *testing.T, settings ...setting) *world {
	t.Helper()
	w := &world{
		docker: newFakeDocker(), registry: newFakeRegistry(), conntrack: &fakes.Conntrack{},
		netns: &fakes.Netns{}, tcp: &fakes.Prober{}, log: &logtest.Logger{},
		clock: clock.NewStopped(time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)),
	}
	options := DefaultOptions
	options.HealthPoll = time.Millisecond
	deps := Deps{
		Docker: NewClient(httpx.StreamingHandlerTransport(w.docker)), Netns: w.netns, TCP: w.tcp,
		Options: options, Log: w.log, Clock: w.clock,
	}
	for _, apply := range settings {
		apply(&deps)
	}
	w.clock = deps.Clock
	// Its retries waited out at once: how long they wait is the registry
	// package's to prove, and a stopped clock would hold a pass on them.
	deps.Registry = registry.New(httpx.HandlerTransport(w.registry), deps.Auth, clock.NewInstant(time.Time{}))
	w.source = NewSource(deps)
	w.daemon = daemon.New(daemon.Deps{Sources: []source.Source{w.source}, Conntrack: w.conntrack, Options: daemon.Defaults, Log: w.log, Clock: w.clock})
	t.Cleanup(w.source.Prefetched)
	return w
}

// once is a pass, bounded so a gate that never settles fails the test
// rather than hanging it.
func (w *world) once(t *testing.T) []decide.Kind {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	outcomes, err := w.daemon.Once(ctx)
	if err != nil {
		t.Fatal(err)
	}
	kinds := make([]decide.Kind, len(outcomes))
	for i, outcome := range outcomes {
		kinds[i] = outcome.Action.Kind
	}
	return kinds
}

func (w *world) expect(t *testing.T, want ...decide.Kind) {
	t.Helper()
	if got := w.once(t); !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// opted is a container running current whose tag resolves to desired.
//
// It was created from the image whose manifest digest is current, reachable
// by its image id (stable) and by the tag (until a pull moves it), and a
// pull lands desired. A recreated container comes up healthy unless a test
// says otherwise, so health=docker commits with no time passing.
func (w *world) opted(name string, current, desired model.Digest, labels map[string]string) {
	imageID := "config-" + string(current)
	w.docker.add(container(name, func(c *model.RunningContainer) {
		c.ImageID = imageID
		c.Spec.Bindings = bound(port(8080, 8080, model.TCP))
		for key, value := range labels {
			c.Spec.Labels[key] = value
		}
	}))
	w.registry.set("app:latest", desired)
	w.docker.with(func(f *fakeDocker) {
		f.images[imageID] = current
		f.images["app:latest"] = current
		f.pullLands["app:latest"] = desired
		f.healthByName[name] = model.Healthy
	})
}

// moves is the registry's tag, and what a pull of it lands, moving on.
func (w *world) moves(digest model.Digest) {
	w.registry.set("app:latest", digest)
	w.docker.with(func(f *fakeDocker) { f.pullLands["app:latest"] = digest })
}

var dockerHealth = map[string]string{HealthLabel: "docker", HealthTimeoutLabel: "1"}

func labels(extra ...map[string]string) map[string]string {
	all := map[string]string{}
	for _, labels := range extra {
		for key, value := range labels {
			all[key] = value
		}
	}
	return all
}

func (f *fakeDocker) pulled() []string {
	return read(f, func(f *fakeDocker) []string {
		var refs []string
		for _, p := range f.pulls {
			refs = append(refs, p.ref)
		}
		return refs
	})
}

func (f *fakeDocker) removedAny(name string) bool {
	return read(f, func(f *fakeDocker) bool {
		return slices.ContainsFunc(f.removes, func(r removed) bool { return r.container == name })
	})
}

func (f *fakeDocker) restored(name string) bool {
	return read(f, func(f *fakeDocker) bool {
		return slices.Contains(f.renames, renamed{RollbackName(name), name})
	})
}

func (f *fakeDocker) created() []string {
	return read(f, func(f *fakeDocker) []string {
		var names []string
		for _, c := range f.creates {
			names = append(names, c.Name)
		}
		return names
	})
}

func (f *fakeDocker) isRunning(name string) bool {
	return read(f, func(f *fakeDocker) bool { return f.find(name) >= 0 })
}

func TestAContainerAlreadyOnTheLatestImageIsKeptAndNothingPulled(t *testing.T) {
	w := newWorld(t)
	w.opted("web", "sha256:same", "sha256:same", nil)
	w.expect(t, decide.Keep)
	w.source.Prefetched()
	if pulls := w.docker.pulled(); len(pulls) != 0 {
		t.Errorf("pulled %v", pulls)
	}
}

func TestAnIdleContainerWithANewerImageIsPulledRecreatedAndCommittedOnceHealthy(t *testing.T) {
	w := newWorld(t)
	w.opted("web", "sha256:old", "sha256:new", dockerHealth)

	// Three passes to clear the default debounce of three idle samples.
	w.expect(t, decide.Defer)
	w.expect(t, decide.Defer)
	w.expect(t, decide.Update)
	w.source.Prefetched()

	// Twice: the background prefetch when the newer digest appeared, then the
	// authoritative pull at swap time (cache-warm, so cheap on a real host).
	if pulls := w.docker.pulled(); !slices.Equal(pulls, []string{"app:latest", "app:latest"}) {
		t.Errorf("pulled %v", pulls)
	}
	// Rename-not-delete: old parked, new created, old removed on commit.
	renames := read(w.docker, func(f *fakeDocker) []renamed { return f.renames })
	if len(renames) == 0 || renames[0] != (renamed{"web", RollbackName("web")}) {
		t.Errorf("renamed %v", renames)
	}
	if created := w.docker.created(); !slices.Equal(created, []string{"web"}) {
		t.Errorf("created %v", created)
	}
	if !w.docker.removedAny(RollbackName("web")) {
		t.Error("the parked container was not removed")
	}
	if !w.log.Contains("web: updated to sha256:new and it is healthy") {
		t.Errorf("said %q", w.log.Text())
	}
	// And the next pass finds it on the new image: nothing more to do.
	w.expect(t, decide.Keep)
}

func TestTheRecreatedContainerIsTheOriginalFaithfully(t *testing.T) {
	w := newWorld(t)
	w.docker.add(container("web", func(c *model.RunningContainer) {
		c.ImageID = "config-sha256:old"
		c.Spec.Labels[IdleSamplesLabel] = "1"
		c.Spec.Labels[HealthLabel] = "docker"
		c.Spec.Env = []string{"A=1"}
		c.Spec.Bindings = bound(port(8080, 80, model.TCP))
		c.Spec.Mounts = []model.Mount{{Type: model.Bind, Source: "/srv", Target: "/srv", ReadOnly: true}, {Type: model.Volume, Source: "data", Target: "/data"}}
		c.Spec.Networks = []model.NetworkAttachment{{Name: "front", Aliases: []string{"web"}}, {Name: "back", Aliases: []string{}, IPv4Address: "10.0.0.5"}}
		c.Spec.NetworkMode = "front"
		c.Spec.RestartPolicy = &model.RestartPolicy{Name: "unless-stopped"}
	}))
	w.registry.set("app:latest", "sha256:new")
	w.docker.with(func(f *fakeDocker) {
		f.images["config-sha256:old"] = "sha256:old"
		f.pullLands["app:latest"] = "sha256:new"
		f.healthByName["web"] = model.Healthy
	})
	original := read(w.docker, func(f *fakeDocker) model.ContainerSpec { return f.running[0].Spec })

	w.expect(t, decide.Update)

	created := read(w.docker, func(f *fakeDocker) []model.ContainerSpec { return f.creates })
	// The create carries the first network; the second is connected after.
	want := original
	want.Networks = original.Networks[:1]
	if len(created) != 1 || !reflect.DeepEqual(created[0], want) {
		t.Fatalf("created %+v\nwant    %+v", created, want)
	}
	connects := read(w.docker, func(f *fakeDocker) []connected { return f.connects })
	if len(connects) != 1 || connects[0].container != "web" || connects[0].network.Name != "back" || connects[0].network.IPv4Address != "10.0.0.5" {
		t.Errorf("connected %+v", connects)
	}
	stops := read(w.docker, func(f *fakeDocker) []stopped { return f.stops })
	if len(stops) != 1 || stops[0] != (stopped{"web", DefaultOptions.DefaultGraceSeconds}) {
		t.Errorf("stopped %+v", stops)
	}
}

// The bug a verbatim copy had for real: swapped onto v2, the container still
// ran v1's CMD, with v1's ENV, labels and HEALTHCHECK. Recreated, it takes
// v2's defaults, and keeps everything it set itself.
func TestASwapRunsTheNewImagesCmdAndEnvAndKeepsTheContainersOwn(t *testing.T) {
	v1 := ImageDefaults{
		Env: []string{"PATH=/v1/bin", "APP_VERSION=1", "APP_MODE=prod"}, Cmd: []string{"serve", "--v1"}, Entrypoint: []string{"/entry"},
		Labels: map[string]string{"org.opencontainers.image.version": "1"}, Exposed: []string{"8080/tcp", "9000/tcp"},
		WorkingDir: "/v1", Healthcheck: &model.Healthcheck{Test: []string{"CMD", "/v1/check"}, Interval: 30 * time.Second},
	}
	v2 := ImageDefaults{
		Env: []string{"PATH=/v2/bin", "APP_VERSION=2", "APP_MODE=prod"}, Cmd: []string{"serve", "--v2"}, Entrypoint: []string{"/entry"},
		Labels: map[string]string{"org.opencontainers.image.version": "2"}, Exposed: []string{"8080/tcp"},
		WorkingDir: "/v2", Healthcheck: &model.Healthcheck{Test: []string{"CMD", "/v2/check"}, Interval: 30 * time.Second},
	}
	w := newWorld(t)
	w.opted("web", "sha256:old", "sha256:new", labels(dockerHealth, map[string]string{IdleSamplesLabel: "1"}))
	w.docker.with(func(f *fakeDocker) {
		f.configs["sha256:old"], f.configs["sha256:new"] = v1, v2
		// As Docker stored it at `docker run -e APP_MODE=dev -e TZ=UTC
		// --health-interval 5s -p 8080:8080` on v1.
		own := f.running[0].Spec
		own.Env = []string{"APP_MODE=dev", "TZ=UTC"}
		own.Healthcheck = &model.Healthcheck{Interval: 5 * time.Second}
		f.running[0].Spec = merged(own, v1)
	})

	w.expect(t, decide.Update)

	after := read(w.docker, func(f *fakeDocker) model.ContainerSpec { return f.running[f.find("web")].Spec })
	expectJSON(t, "Cmd", after.Cmd, `["serve","--v2"]`)
	expectJSON(t, "Entrypoint", after.Entrypoint, `["/entry"]`)
	expectJSON(t, "Env", after.Env, `["APP_MODE=dev","TZ=UTC","PATH=/v2/bin","APP_VERSION=2"]`)
	expectJSON(t, "Labels", after.Labels, `{"org.opencontainers.image.version":"2","tidewaiter.autoupdate":"registry","tidewaiter.health":"docker","tidewaiter.health-timeout":"1","tidewaiter.idle-samples":"1"}`)
	expectJSON(t, "Exposed", after.Exposed, `["8080/tcp"]`)
	expectJSON(t, "WorkingDir", after.WorkingDir, `"/v2"`)
	if want := (&model.Healthcheck{Test: []string{"CMD", "/v2/check"}, Interval: 5 * time.Second}); !reflect.DeepEqual(after.Healthcheck, want) {
		t.Errorf("healthcheck %+v", after.Healthcheck)
	}
	if !reflect.DeepEqual(after.Bindings, bound(port(8080, 8080, model.TCP))) {
		t.Errorf("bindings %+v", after.Bindings)
	}
	// On the wire: only what the container set itself.
	sent := read(w.docker, func(f *fakeDocker) map[string]any { return f.payloads[0] })
	expectJSON(t, "sent Env", sent["Env"], `["APP_MODE=dev","TZ=UTC"]`)
	for _, field := range []string{"Cmd", "Entrypoint", "WorkingDir"} {
		if value, ok := sent[field]; ok {
			t.Errorf("sent %s %s", field, jsonOf(value))
		}
	}
}

// Without the running image there is no telling the container's own
// settings from its image's, and copying them all is the bug: no swap, and
// nothing pinned.
func TestASwapIsRefusedWhenTheImageTheContainerRunsIsGone(t *testing.T) {
	w := newWorld(t)
	w.opted("web", "sha256:old", "sha256:new", labels(dockerHealth, map[string]string{IdleSamplesLabel: "1"}))
	w.docker.with(func(f *fakeDocker) { delete(f.images, "config-sha256:old") })

	w.expect(t, decide.Update)

	if stops := read(w.docker, func(f *fakeDocker) []stopped { return f.stops }); len(stops) != 0 || len(w.docker.created()) != 0 {
		t.Errorf("stopped %v, created %v", stops, w.docker.created())
	}
	if !w.log.Contains("web: the image it was created from (config-sha256:old) is no longer here") {
		t.Errorf("said %q", w.log.Text())
	}
	// Not pinned: once the image is back, the update goes ahead.
	w.docker.with(func(f *fakeDocker) { f.images["config-sha256:old"] = "sha256:old" })
	w.expect(t, decide.Update)
	if created := w.docker.created(); !slices.Equal(created, []string{"web"}) {
		t.Errorf("created %v", created)
	}
}

// The running image's defaults are read before anything is decided, so a
// lookup that fails leaves the container exactly as it was.
func TestAnImageLookupThatFailsTouchesNothing(t *testing.T) {
	w := newWorld(t)
	w.opted("web", "sha256:old", "sha256:new", labels(dockerHealth, map[string]string{IdleSamplesLabel: "1"}))
	w.docker.with(func(f *fakeDocker) { f.failImage = "Docker is restarting" })

	w.expect(t, decide.Keep)

	if stops := read(w.docker, func(f *fakeDocker) []stopped { return f.stops }); len(stops) != 0 || len(w.docker.created()) != 0 {
		t.Errorf("stopped %v, created %v", stops, w.docker.created())
	}
	if !w.log.Contains("could not read the running image digest") {
		t.Errorf("said %q", w.log.Text())
	}
}

// A `-p 80` comes back as a `-p 80`: Docker picks the host port again,
// rather than the recreated container publishing nothing.
func TestASwapKeepsPublishingAPortWhoseHostPortDockerPicks(t *testing.T) {
	w := newWorld(t)
	w.opted("web", "sha256:old", "sha256:new", labels(dockerHealth, map[string]string{IdleSamplesLabel: "1"}))
	ephemeral := []model.PortBinding{{PublishedPort: port(0, 80, model.TCP)}, {HostIP: "127.0.0.1", HostRange: "18000-18010", PublishedPort: port(0, 81, model.TCP)}}
	w.docker.with(func(f *fakeDocker) { f.running[0].Spec.Bindings = ephemeral })

	w.expect(t, decide.Update)

	sent := read(w.docker, func(f *fakeDocker) map[string]any { return f.payloads[0] })
	expectJSON(t, "PortBindings", sent["HostConfig"].(map[string]any)["PortBindings"], `{"80/tcp":[{}],"81/tcp":[{"HostIp":"127.0.0.1","HostPort":"18000-18010"}]}`)
	if after := read(w.docker, func(f *fakeDocker) model.ContainerSpec { return f.running[f.find("web")].Spec }); !reflect.DeepEqual(after.Bindings, ephemeral) {
		t.Errorf("bindings %+v", after.Bindings)
	}
}

func TestRollsBackAndPinsOutWhenTheNewImageIsUnhealthy(t *testing.T) {
	w := newWorld(t)
	w.opted("web", "sha256:old", "sha256:bad", dockerHealth)
	w.docker.with(func(f *fakeDocker) { f.healthByName["web"] = model.Unhealthy })

	w.once(t)
	w.once(t)
	w.once(t) // update -> unhealthy -> rollback

	if len(w.docker.created()) == 0 {
		t.Fatal("nothing was created")
	}
	// The new one removed, the parked old one given its name back.
	if !w.docker.removedAny("web") || !w.docker.restored("web") {
		t.Errorf("removes %v, renames %v", read(w.docker, func(f *fakeDocker) []removed { return f.removes }), read(w.docker, func(f *fakeDocker) []renamed { return f.renames }))
	}
	if !w.log.Contains("rolled back") || !w.log.Contains("Docker reports the container unhealthy") {
		t.Errorf("said %q", w.log.Text())
	}
	// The tag still resolves to the bad digest, so it stays pinned.
	w.expect(t, decide.Pinned)
}

// For health=docker the event stream settles the gate, not the poll: the
// clock never moves and the container stays "starting", so the poll can never
// conclude - if the update commits, the health_status event drove it.
func TestCommitsOnAHealthyEventWithoutWaitingOutThePoll(t *testing.T) {
	w := newWorld(t, pollEvery(time.Minute))
	w.opted("web", "sha256:old", "sha256:new", labels(dockerHealth, map[string]string{HealthTimeoutLabel: "600"}))
	w.docker.with(func(f *fakeDocker) { f.healthByName["web"] = model.Starting })
	// Queued before the pass; the watch takes it as soon as the gate opens.
	w.docker.emit(Event{Action: "health_status: healthy", Container: "web"})

	w.once(t)
	w.once(t)
	w.expect(t, decide.Update)

	if !w.docker.removedAny(RollbackName("web")) {
		t.Error("not committed")
	}
	// The gate let go of its stream on the way out.
	await.Until(t, "the event stream to close", func() bool { return w.docker.open() == 0 })
}

func TestRollsBackOnAnUnhealthyEvent(t *testing.T) {
	w := newWorld(t, pollEvery(time.Minute))
	w.opted("web", "sha256:old", "sha256:bad", labels(dockerHealth, map[string]string{HealthTimeoutLabel: "600"}))
	w.docker.with(func(f *fakeDocker) { f.healthByName["web"] = model.Starting })
	w.docker.emit(Event{Action: "health_status: unhealthy", Container: "web"})

	w.once(t)
	w.once(t)
	w.once(t)

	if !w.log.Contains("rolled back") || !w.docker.restored("web") {
		t.Errorf("said %q", w.log.Text())
	}
}

// A stream that drops mid-gate is opened again, and an event on the new one
// still settles it.
func TestReopensAnEventStreamThatDropsDuringTheGate(t *testing.T) {
	stopped := clock.NewStopped(time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC))
	w := newWorld(t, pollEvery(time.Minute), withClock(stopped))
	w.opted("web", "sha256:old", "sha256:new", labels(dockerHealth, map[string]string{HealthTimeoutLabel: "600", IdleSamplesLabel: "1"}))
	w.docker.with(func(f *fakeDocker) { f.healthByName["web"] = model.Starting })

	passed := make(chan []decide.Kind)
	go func() { passed <- w.once(t) }()

	await.Until(t, "the gate to watch for events", func() bool { return w.docker.open() == 1 })
	w.docker.dropStreams()
	// The poll waiting a minute, the watch a second before it looks again.
	if err := stopped.BlockUntil(t.Context(), 2); err != nil {
		t.Fatal(err)
	}
	stopped.Advance(time.Second)
	await.Until(t, "the stream to be opened again", func() bool { return w.docker.open() == 1 && w.docker.everOpened() == 2 })
	w.docker.emit(Event{Action: "health_status: healthy", Container: "web"})

	if got := <-passed; !slices.Equal(got, []decide.Kind{decide.Update}) {
		t.Fatalf("got %v", got)
	}
	if !w.docker.removedAny(RollbackName("web")) {
		t.Error("not committed")
	}
}

// The port-connect path end to end, the one that failed on a live host:
// confirmed by a real connect to the published host port, so there is no
// PID or netns timing to race.
func TestCommitsAPortConnectUpdateOnceThePublishedHostPortAccepts(t *testing.T) {
	w := newWorld(t)
	w.docker.add(container("web", func(c *model.RunningContainer) {
		c.ImageID = "config-sha256:old"
		c.Spec.Labels[IdleSamplesLabel] = "2"
		c.Spec.Labels[HealthLabel] = "port-connect"
		c.Spec.Labels[HealthTimeoutLabel] = "5"
		c.Spec.Bindings = bound(port(18080, 80, model.TCP))
	}))
	w.registry.set("app:latest", "sha256:new")
	w.docker.with(func(f *fakeDocker) {
		f.images["config-sha256:old"] = "sha256:old"
		f.images["app:latest"] = "sha256:old"
		f.pullLands["app:latest"] = "sha256:new"
	})
	w.tcp.Listening(18080)

	w.expect(t, decide.Defer)
	w.expect(t, decide.Update)

	if !slices.Contains(w.tcp.ProbedPorts(), 18080) {
		t.Errorf("probed %v", w.tcp.ProbedPorts())
	}
	if !w.docker.removedAny(RollbackName("web")) {
		t.Error("not committed")
	}
}

// ticking is a clock where time passes the moment anyone waits.
type ticking struct{ *clock.Stopped }

func (c ticking) After(d time.Duration) <-chan time.Time {
	wait := c.Stopped.After(d)
	c.Advance(d)
	return wait
}

// Leaning toward trust: a port that never accepts is not an active failure,
// so at the timeout the update commits anyway - a flaky or slow probe must
// not veto a good update.
func TestCommitsAtTheTimeoutEvenWhenThePortNeverAccepts(t *testing.T) {
	w := newWorld(t, pollEvery(250*time.Millisecond), withClock(ticking{clock.NewStopped(time.Unix(1_000_000, 0))}))
	w.docker.add(container("web", func(c *model.RunningContainer) {
		c.ImageID = "config-sha256:old"
		c.Spec.Labels[IdleSamplesLabel] = "1"
		c.Spec.Labels[HealthLabel] = "port-connect"
		c.Spec.Labels[HealthTimeoutLabel] = "1"
		c.Spec.Bindings = bound(port(18080, 80, model.TCP))
	}))
	w.registry.set("app:latest", "sha256:new")
	w.docker.with(func(f *fakeDocker) {
		f.images["config-sha256:old"] = "sha256:old"
		f.pullLands["app:latest"] = "sha256:new"
	})

	w.once(t)

	if !w.docker.removedAny(RollbackName("web")) || w.docker.restored("web") {
		t.Error("expected a commit")
	}
}

// uptime's active failure: a new container that exits is a genuine failure,
// rolled back even under the trust-the-update rule.
func TestRollsBackWhenTheNewContainerExits(t *testing.T) {
	w := newWorld(t)
	w.docker.add(container("web", func(c *model.RunningContainer) {
		c.ImageID = "config-sha256:old"
		c.Spec.Labels[IdleSamplesLabel] = "1"
		c.Spec.Labels[HealthLabel] = "uptime"
		c.Spec.Labels[HealthTimeoutLabel] = "5"
		c.Spec.Bindings = bound(port(18080, 80, model.TCP))
	}))
	w.registry.set("app:latest", "sha256:new")
	w.docker.with(func(f *fakeDocker) {
		f.images["config-sha256:old"] = "sha256:old"
		f.pullLands["app:latest"] = "sha256:new"
		f.runningByName["web"] = false
	})

	w.once(t)

	if !w.docker.restored("web") {
		t.Error("not rolled back")
	}
}

// If a swap step fails BEFORE the old container was renamed away, the undo
// must not force-remove the still-good original - which a blind rollback did,
// taking the service down.
func TestAStopFailureAtTheStartOfASwapNeverRemovesTheOriginal(t *testing.T) {
	w := newWorld(t)
	w.opted("web", "sha256:old", "sha256:new", dockerHealth)
	w.docker.with(func(f *fakeDocker) { f.failOp[Stop] = "Docker socket hiccup" })

	w.once(t)
	w.once(t)
	w.once(t)

	if w.docker.removedAny("web") {
		t.Error("removed the original")
	}
	if renames := read(w.docker, func(f *fakeDocker) []renamed { return f.renames }); len(renames) != 0 {
		t.Errorf("renamed %v", renames)
	}
	if created := w.docker.created(); len(created) != 0 {
		t.Errorf("created %v", created)
	}
	if !w.log.Contains("undoing what was done") || !w.log.Contains("Docker socket hiccup") {
		t.Errorf("said %q", w.log.Text())
	}
}

// A create failing after the old one was parked: the parked one is given its
// name back and started, and nothing is left wearing the name.
func TestACreateFailureRestoresTheParkedOriginal(t *testing.T) {
	w := newWorld(t)
	w.opted("web", "sha256:old", "sha256:new", labels(dockerHealth, map[string]string{IdleSamplesLabel: "1"}))
	w.docker.with(func(f *fakeDocker) { f.failOp[Create] = "temporary disk pressure" })

	w.once(t)

	if !w.docker.restored("web") || w.docker.removedAny("web") {
		t.Errorf("renames %v", read(w.docker, func(f *fakeDocker) []renamed { return f.renames }))
	}
	if starts := read(w.docker, func(f *fakeDocker) []string { return f.starts }); !slices.Equal(starts, []string{"web"}) {
		t.Errorf("started %v", starts)
	}
	if !w.docker.isRunning("web") || w.docker.isRunning(RollbackName("web")) {
		t.Error("the original does not wear its name")
	}
}

// A transient swap failure must NOT pin the tag: pinning means "the image is
// bad", and a bounced API call says nothing about the image.
func TestATransientSwapFailureDoesNotPinTheTagOut(t *testing.T) {
	w := newWorld(t)
	w.opted("web", "sha256:old", "sha256:new", dockerHealth)
	w.docker.with(func(f *fakeDocker) { f.failOp[Create] = "temporary disk pressure" })

	w.once(t)
	w.once(t)
	w.once(t)

	w.docker.with(func(f *fakeDocker) { delete(f.failOp, Create) })
	w.expect(t, decide.Update)
}

// A DETERMINISTIC swap failure (a broken image, not a blip) must not retry
// forever: after a few in a row on one digest it pins out, and the service
// stays up throughout.
func TestBacksOffAndPinsOutAfterRepeatedSwapFailuresOnOneDigest(t *testing.T) {
	w := newWorld(t)
	w.opted("web", "sha256:old", "sha256:new", labels(dockerHealth, map[string]string{IdleSamplesLabel: "1"}))
	w.docker.with(func(f *fakeDocker) { f.failOp[Start] = "exec: entrypoint not found" })

	for range source.MaxSwapFailures {
		w.expect(t, decide.Update)
	}
	w.expect(t, decide.Pinned)
	if !w.log.Contains("pinning it out") {
		t.Errorf("said %q", w.log.Text())
	}
	if !w.docker.isRunning("web") {
		t.Error("the original did not survive")
	}

	// A new digest clears the back-off.
	w.moves("sha256:fixed")
	w.docker.with(func(f *fakeDocker) { delete(f.failOp, Start) })
	w.expect(t, decide.Update)
}

func TestStopsPinningOnceTheTagMovesToANewDigest(t *testing.T) {
	w := newWorld(t)
	w.opted("web", "sha256:old", "sha256:bad", dockerHealth)
	w.docker.with(func(f *fakeDocker) { f.healthByName["web"] = model.Unhealthy })
	w.once(t)
	w.once(t)
	w.once(t)
	w.expect(t, decide.Pinned)

	// A genuinely new image appears and comes up healthy. Only the registry
	// and what a pull lands move; the container is still on the old image.
	w.moves("sha256:fixed")
	w.docker.with(func(f *fakeDocker) { f.healthByName["web"] = model.Healthy })
	// Pin cleared, and still idle from before, so it updates at once.
	w.expect(t, decide.Update)
}

// Private-registry credentials reach the pull, not just the digest lookup.
func TestPassesXRegistryAuthThroughToThePull(t *testing.T) {
	w := newWorld(t, withAuth(func(host string) string {
		if host == registry.DockerHub {
			return "base64-creds"
		}
		return ""
	}))
	w.opted("web", "sha256:old", "sha256:new", dockerHealth)

	w.once(t)
	w.once(t)
	w.once(t)
	w.source.Prefetched()

	// The background prefetch and the swap's own pull alike.
	pulls := read(w.docker, func(f *fakeDocker) []pull { return f.pulls })
	if !slices.Equal(pulls, []pull{{"app:latest", "base64-creds"}, {"app:latest", "base64-creds"}}) {
		t.Errorf("pulled %v", pulls)
	}
}

func TestAPullThatFailsLeavesTheContainerRunning(t *testing.T) {
	w := newWorld(t)
	w.opted("web", "sha256:old", "sha256:new", labels(dockerHealth, map[string]string{IdleSamplesLabel: "1"}))
	w.docker.with(func(f *fakeDocker) { f.failPull = "manifest unknown" })

	w.once(t)
	w.source.Prefetched()

	if !w.log.Contains("web: FAILED to pull app:latest") || !w.log.Contains("manifest unknown") {
		t.Errorf("said %q", w.log.Text())
	}
	if stops := read(w.docker, func(f *fakeDocker) []stopped { return f.stops }); len(stops) != 0 {
		t.Errorf("stopped %v", stops)
	}
}

func TestWarnsWhenRecreatingAContainerNetworkModeButStillUpdates(t *testing.T) {
	w := newWorld(t)
	w.opted("web", "sha256:old", "sha256:new", dockerHealth)
	w.docker.with(func(f *fakeDocker) { f.running[0].Spec.NetworkMode = "container:sidecar" })

	w.once(t)
	w.once(t)
	w.expect(t, decide.Update)

	if !w.log.Contains("shares another container's netns") {
		t.Errorf("said %q", w.log.Text())
	}
}

func TestRefusesToRecreateAContainerWithAnAnonymousVolume(t *testing.T) {
	w := newWorld(t)
	w.opted("web", "sha256:old", "sha256:new", labels(dockerHealth, map[string]string{IdleSamplesLabel: "1"}))
	w.docker.with(func(f *fakeDocker) {
		f.running[0].Spec.Mounts = []model.Mount{{Type: model.Volume, Source: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Target: "/var/lib/legacy"}}
	})

	w.once(t)

	if !w.log.Contains("anonymous volume at /var/lib/legacy") {
		t.Errorf("said %q", w.log.Text())
	}
	if stops := read(w.docker, func(f *fakeDocker) []stopped { return f.stops }); len(stops) != 0 {
		t.Errorf("stopped %v", stops)
	}
}

func TestAContainerWithAnUnrecognisedAutoupdateValueIsLeftAlone(t *testing.T) {
	w := newWorld(t)
	// Opted in by the key but with a typo: listed, since the listing filters
	// on the key, but never touched.
	w.opted("web", "sha256:old", "sha256:new", map[string]string{AutoupdateLabel: "registy"})

	w.expect(t, decide.Keep)
	w.once(t)

	w.source.Prefetched()
	if len(w.docker.pulled()) != 0 || len(w.docker.created()) != 0 {
		t.Error("touched it")
	}
	// A standing misconfiguration is said once, not every pass.
	said := 0
	for _, line := range w.log.Lines() {
		if line == "web: tidewaiter.autoupdate: 'registy' is not a known policy (registry, local); ignoring this container" {
			said++
		}
	}
	if said != 1 {
		t.Errorf("said it %d times: %q", said, w.log.Text())
	}
}

func TestTheLocalPolicyUpdatesFromTheLocalImageWithoutTheRegistryOrAPull(t *testing.T) {
	w := newWorld(t)
	// Running config-old; the tag's LOCAL image has been rebuilt.
	w.docker.add(container("web", func(c *model.RunningContainer) {
		c.ImageID = "config-old"
		c.Spec.Labels = map[string]string{AutoupdateLabel: "local", HealthLabel: "docker", HealthTimeoutLabel: "1"}
		c.Spec.Bindings = bound(port(8080, 8080, model.TCP))
	}))
	w.docker.with(func(f *fakeDocker) {
		f.images["config-old"] = "sha256:old"
		f.images["app:latest"] = "sha256:built"
		f.healthByName["web"] = model.Healthy
	})

	w.once(t)
	w.once(t)
	w.expect(t, decide.Update)
	w.source.Prefetched()

	if len(w.docker.pulled()) != 0 || len(w.registry.asked()) != 0 {
		t.Errorf("pulled %v, asked the registry %v", w.docker.pulled(), w.registry.asked())
	}
	if created := w.docker.created(); !slices.Equal(created, []string{"web"}) {
		t.Errorf("created %v", created)
	}
}

func TestTheLocalPolicyKeepsAContainerWhoseTagIsNotHere(t *testing.T) {
	w := newWorld(t)
	w.docker.add(container("web", func(c *model.RunningContainer) {
		c.Spec.Labels = map[string]string{AutoupdateLabel: "local"}
	}))
	w.expect(t, decide.Keep)
}

func TestARegistryThatCannotBeReachedLeavesTheContainerAlone(t *testing.T) {
	w := newWorld(t)
	w.opted("web", "sha256:old", "sha256:new", nil)
	w.registry.fail("down for maintenance")

	outcomes, err := w.daemon.Once(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if outcomes[0].Action.Reason != "registry unreachable" || outcomes[0].Busy != nil {
		t.Errorf("got %+v", outcomes[0])
	}
	if !w.log.Contains("web: could not resolve registry digest") || !w.log.Contains("down for maintenance") {
		t.Errorf("said %q", w.log.Text())
	}
}

func TestAListingThatFailsFailsThePass(t *testing.T) {
	w := newWorld(t)
	w.docker.with(func(f *fakeDocker) { f.failWith = "Docker is restarting" })
	if _, err := w.daemon.Once(t.Context()); err == nil {
		t.Error("expected the pass to fail")
	}
}

func TestDefersWhileTheContainerHasLiveFlows(t *testing.T) {
	w := newWorld(t)
	w.opted("web", "sha256:old", "sha256:new", nil)
	w.conntrack.Flows(fakes.Flow(8080))

	for range 5 {
		w.expect(t, decide.Defer)
	}
	w.source.Prefetched()
	// Busy, so it never swaps - but it DOES warm the cache, once.
	if len(w.docker.created()) != 0 {
		t.Error("swapped while busy")
	}
	if pulls := w.docker.pulled(); !slices.Equal(pulls, []string{"app:latest"}) {
		t.Errorf("pulled %v", pulls)
	}
}

func TestRequiresNConsecutiveIdleSamplesBeforeUpdating(t *testing.T) {
	w := newWorld(t)
	w.opted("web", "sha256:old", "sha256:new", labels(dockerHealth, map[string]string{IdleSamplesLabel: "3"}))
	w.expect(t, decide.Defer)
	w.expect(t, decide.Defer)
	w.expect(t, decide.Update)
}

func TestATransientFlowResetsTheStreakButDoesNotCancelTheUpdate(t *testing.T) {
	w := newWorld(t)
	w.opted("web", "sha256:old", "sha256:new", labels(dockerHealth, map[string]string{IdleSamplesLabel: "2"}))

	w.expect(t, decide.Defer) // idle 1
	w.conntrack.Flows(fakes.Flow(8080))
	w.expect(t, decide.Defer) // busy - resets
	w.conntrack.Flows()
	w.expect(t, decide.Defer) // idle 1 again
	w.expect(t, decide.Update)
}

// A flow that has finished (TIME_WAIT lingers two minutes) is no client.
func TestAClosingFlowIsNotActivity(t *testing.T) {
	w := newWorld(t)
	w.opted("web", "sha256:old", "sha256:new", labels(dockerHealth, map[string]string{IdleSamplesLabel: "1"}))
	w.conntrack.Flows(detect.ConntrackEntry{Protocol: model.TCP, DestinationPort: 8080, Assured: true, State: "TIME_WAIT"})
	w.expect(t, decide.Update)
}

func TestAConntrackTableThatCannotBeReadIsBusy(t *testing.T) {
	w := newWorld(t)
	w.opted("web", "sha256:old", "sha256:new", labels(dockerHealth, map[string]string{IdleSamplesLabel: "1"}))
	w.conntrack.FailWith(errors.New("conntrack: operation not permitted"))

	w.expect(t, decide.Defer)
	if !w.log.Contains("conntrack") {
		t.Errorf("said %q", w.log.Text())
	}
}

func TestANetioStatsReadThatFailsIsBusy(t *testing.T) {
	w := newWorld(t)
	w.opted("web", "sha256:old", "sha256:new", map[string]string{DetectorLabel: "netio"})
	w.docker.with(func(f *fakeDocker) { f.netBytesFail = "container not running" })

	w.expect(t, decide.Defer)
	if len(w.docker.created()) != 0 {
		t.Error("swapped")
	}
	if !w.log.Contains("web: activity detector 'netio' failed") {
		t.Errorf("said %q", w.log.Text())
	}
}

func TestNetioReadsARateAcrossPasses(t *testing.T) {
	ticks := clock.NewStopped(time.Unix(1_000_000, 0))
	w := newWorld(t, withClock(ticks))
	w.opted("web", "sha256:old", "sha256:new", labels(dockerHealth, map[string]string{DetectorLabel: "netio", IdleSamplesLabel: "1"}))

	// The first sample has nothing to compare with: busy.
	w.expect(t, decide.Defer)
	// A minute and a megabyte later: busy.
	ticks.Advance(time.Minute)
	w.docker.with(func(f *fakeDocker) { f.netBytes["id-web"] = 1 << 20 * 60 })
	w.expect(t, decide.Defer)
	// A quiet minute: idle.
	ticks.Advance(time.Minute)
	w.expect(t, decide.Update)
}

func TestAPortsOverrideThatSelectsNothingIsBusy(t *testing.T) {
	w := newWorld(t)
	w.opted("web", "sha256:old", "sha256:new", labels(dockerHealth, map[string]string{IdleSamplesLabel: "1", PortsLabel: "9999"}))
	w.expect(t, decide.Defer)
	if !w.log.Contains("web: tidewaiter.ports: '9999' is not a port this container publishes; ignoring it") {
		t.Errorf("said %q", w.log.Text())
	}
}

func TestAPortsOverrideScopesTheActivityGate(t *testing.T) {
	w := newWorld(t)
	w.opted("web", "sha256:old", "sha256:new", labels(dockerHealth, map[string]string{IdleSamplesLabel: "1", PortsLabel: "8080"}))
	w.docker.with(func(f *fakeDocker) {
		f.running[0].Spec.Bindings = append(f.running[0].Spec.Bindings, bound(port(9100, 9100, model.TCP))...)
	})
	// The metrics port is scraped all the time; nobody is on the service.
	w.conntrack.Flows(fakes.Flow(9100))
	w.expect(t, decide.Update)
}

func TestPullsInTheBackgroundWhileTheContainerIsStillBusy(t *testing.T) {
	w := newWorld(t)
	w.opted("web", "sha256:old", "sha256:new", nil)
	w.conntrack.Flows(fakes.Flow(8080))

	w.expect(t, decide.Defer)
	w.source.Prefetched()
	if len(w.docker.created()) != 0 {
		t.Error("swapped while busy")
	}
	if pulls := w.docker.pulled(); !slices.Equal(pulls, []string{"app:latest"}) {
		t.Errorf("pulled %v", pulls)
	}
	if !w.log.Contains("web: prefetching app:latest in the background") {
		t.Errorf("said %q", w.log.Text())
	}

	// Deduplicated: further busy passes on the same digest do not pull again.
	w.once(t)
	w.once(t)
	w.source.Prefetched()
	if pulls := w.docker.pulled(); len(pulls) != 1 {
		t.Errorf("pulled %v", pulls)
	}
}

func TestDoesNotPrefetchWhenAlreadyOnTheLatestLocalOrDryRun(t *testing.T) {
	t.Run("already current", func(t *testing.T) {
		w := newWorld(t)
		w.opted("web", "sha256:same", "sha256:same", nil)
		w.once(t)
		w.source.Prefetched()
		if len(w.docker.pulled()) != 0 {
			t.Error("pulled")
		}
	})
	t.Run("local", func(t *testing.T) {
		w := newWorld(t)
		w.docker.add(container("web", func(c *model.RunningContainer) {
			c.ImageID = "config-old"
			c.Spec.Labels = map[string]string{AutoupdateLabel: "local"}
		}))
		w.docker.with(func(f *fakeDocker) {
			f.images["config-old"] = "sha256:old"
			f.images["app:latest"] = "sha256:built"
		})
		w.once(t)
		w.source.Prefetched()
		if len(w.docker.pulled()) != 0 {
			t.Error("pulled")
		}
	})
	t.Run("dry run", func(t *testing.T) {
		w := newWorld(t, dryRun)
		w.opted("web", "sha256:old", "sha256:new", nil)
		w.once(t)
		w.source.Prefetched()
		if len(w.docker.pulled()) != 0 {
			t.Error("pulled")
		}
	})
}

func TestPrefetchesAgainWhenTheTagMovesWhileBusy(t *testing.T) {
	w := newWorld(t)
	w.opted("web", "sha256:old", "sha256:new", nil)
	w.conntrack.Flows(fakes.Flow(8080))

	w.once(t)
	w.source.Prefetched()
	w.moves("sha256:newer")
	w.once(t)
	w.source.Prefetched()

	if pulls := w.docker.pulled(); len(pulls) != 2 {
		t.Errorf("pulled %v", pulls)
	}
}

func TestAFailedPrefetchNeverPinsAndIsRetriedNextPass(t *testing.T) {
	w := newWorld(t)
	w.opted("web", "sha256:old", "sha256:new", nil)
	w.conntrack.Flows(fakes.Flow(8080))
	w.docker.with(func(f *fakeDocker) { f.failPull = "registry 500" })

	w.expect(t, decide.Defer)
	w.source.Prefetched()
	if !w.log.Contains("web: prefetch of app:latest failed") {
		t.Errorf("said %q", w.log.Text())
	}

	w.docker.with(func(f *fakeDocker) { f.failPull = "" })
	w.expect(t, decide.Defer)
	w.source.Prefetched()
	if pulls := w.docker.pulled(); len(pulls) != 2 {
		t.Errorf("pulled %v", pulls)
	}
}

func TestKeepImagesRetainsThePreviousImageAndPrunesOlderOnes(t *testing.T) {
	w := newWorld(t)
	w.opted("web", "sha256:v1", "sha256:v2", labels(dockerHealth, map[string]string{KeepImagesLabel: "1"}))

	// v1 -> v2: only one previous, which keep-images=1 keeps.
	w.once(t)
	w.once(t)
	w.once(t)
	if removed := read(w.docker, func(f *fakeDocker) []string { return f.removedImages }); len(removed) != 0 {
		t.Errorf("pruned %v", removed)
	}

	// v2 -> v3: now v1 is outside keep-images=1, and is pruned by its
	// repository digest.
	w.moves("sha256:v3")
	w.expect(t, decide.Update)
	if removed := read(w.docker, func(f *fakeDocker) []string { return f.removedImages }); !slices.Equal(removed, []string{"app@sha256:v1"}) {
		t.Errorf("pruned %v", removed)
	}
}

func TestLogsABaselineAdoptionOnFirstSight(t *testing.T) {
	w := newWorld(t)
	w.opted("web", "sha256:mystery", "sha256:new", nil)
	w.conntrack.Flows(fakes.Flow(8080))
	w.once(t)
	if !w.log.Contains("adopting the running image as the current baseline") {
		t.Errorf("said %q", w.log.Text())
	}
}

func TestDryRunDecidesToUpdateButChangesNothing(t *testing.T) {
	w := newWorld(t, dryRun)
	w.opted("web", "sha256:old", "sha256:new", dockerHealth)

	w.once(t)
	w.once(t)
	w.expect(t, decide.Update)
	w.source.Prefetched()

	if len(w.docker.pulled()) != 0 || len(w.docker.created()) != 0 {
		t.Error("touched it")
	}
	if !w.log.Contains("dry run") {
		t.Errorf("said %q", w.log.Text())
	}
}

func TestSaysNothingToDoOnceThenStaysQuiet(t *testing.T) {
	w := newWorld(t)
	w.opted("web", "sha256:same", "sha256:same", nil)
	w.once(t)
	first := len(w.log.Lines())
	w.once(t)
	if len(w.log.Lines()) != first || !w.log.Contains("up to date") {
		t.Errorf("said %q", w.log.Text())
	}
}

func TestConntrackIsDumpedOncePerPassNotOncePerContainer(t *testing.T) {
	w := newWorld(t)
	w.opted("a", "sha256:same", "sha256:same", nil)
	w.opted("b", "sha256:same", "sha256:same", nil)
	w.once(t)
	if calls := w.conntrack.Calls(); calls != 1 {
		t.Errorf("dumped %d times", calls)
	}
}

func TestHoldsAWorkloadBeforeSwappingIt(t *testing.T) {
	w := newWorld(t)
	w.opted("web", "sha256:old", "sha256:new", labels(dockerHealth, map[string]string{IdleSamplesLabel: "1"}))
	var held []string
	var stoppedFirst bool
	outcomes, err := w.source.Once(t.Context(), source.Pass{Conntrack: []detect.ConntrackEntry{}, Hold: func(workload string) {
		held = append(held, workload)
		stoppedFirst = len(read(w.docker, func(f *fakeDocker) []stopped { return f.stops })) > 0
	}})
	if err != nil || outcomes[0].Action.Kind != decide.Update {
		t.Fatalf("got %+v, %v", outcomes, err)
	}
	if !slices.Equal(held, []string{"web"}) || stoppedFirst {
		t.Errorf("held %v, after stopping: %v", held, stoppedFirst)
	}
	if outcomes[0].Settled {
		t.Error("a container just swapped is not settled")
	}
}

func TestActivityReadsAFailedDumpAsBusyNotAsAnEmptyTable(t *testing.T) {
	w := newWorld(t)
	w.opted("web", "sha256:old", "sha256:new", nil)
	for _, c := range []struct {
		table []detect.ConntrackEntry
		busy  bool
	}{{nil, true}, {[]detect.ConntrackEntry{}, false}} {
		activities, err := w.source.Activity(t.Context(), c.table)
		if err != nil || !slices.Equal(activities, []source.Activity{{Workload: "web", Busy: c.busy}}) {
			t.Errorf("with %v: got %v, %v", c.table, activities, err)
		}
	}
}

func TestActivitySaysNothingAPassSays(t *testing.T) {
	w := newWorld(t)
	w.opted("web", "sha256:old", "sha256:new", map[string]string{DetectorLabel: "netio", IdleSamplesLabel: "lots"})
	w.docker.with(func(f *fakeDocker) { f.netBytesFail = "container not running" })
	if _, err := w.source.Activity(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if lines := w.log.Lines(); len(lines) != 0 {
		t.Errorf("said %q", lines)
	}
}

func TestActivityLeavesOutAContainerThatIsNotOptedIn(t *testing.T) {
	w := newWorld(t)
	w.opted("web", "sha256:old", "sha256:new", map[string]string{AutoupdateLabel: "maybe"})
	if activities, err := w.source.Activity(t.Context(), nil); err != nil || len(activities) != 0 {
		t.Errorf("got %v, %v", activities, err)
	}
}

func TestADetectorThatNeedsNoConntrackStillAnswersWhenTheDumpFailed(t *testing.T) {
	w := newWorld(t)
	w.opted("web", "sha256:old", "sha256:new", map[string]string{DetectorLabel: "none"})
	activities, err := w.source.Activity(t.Context(), nil)
	if err != nil || !slices.Equal(activities, []source.Activity{{Workload: "web", Busy: false}}) {
		t.Errorf("got %v, %v", activities, err)
	}
}

func TestAContainerIsSettledOnceIdleForItsWholeDebounceAndNotWhileBusy(t *testing.T) {
	w := newWorld(t)
	w.opted("web", "sha256:same", "sha256:same", map[string]string{IdleSamplesLabel: "2", DetectorLabel: "none"})
	pass := source.Pass{Conntrack: []detect.ConntrackEntry{}, Hold: func(string) {}}
	for _, settled := range []bool{false, true} {
		outcomes, err := w.source.Once(t.Context(), pass)
		if err != nil || *outcomes[0].Busy || outcomes[0].Settled != settled {
			t.Errorf("got %+v, %v; want settled %v", outcomes, err, settled)
		}
	}
}

func TestAContainerWhoseRegistryDidNotAnswerIsNotSettled(t *testing.T) {
	w := newWorld(t)
	w.opted("web", "sha256:same", "sha256:same", map[string]string{DetectorLabel: "none"})
	w.registry.fail("registry down")
	outcomes, err := w.source.Once(t.Context(), source.Pass{Conntrack: []detect.ConntrackEntry{}, Hold: func(string) {}})
	if err != nil || outcomes[0].Settled || outcomes[0].Busy != nil {
		t.Errorf("got %+v, %v", outcomes, err)
	}
}

func TestListsEachOptedInContainerWithItsStateAndPolicy(t *testing.T) {
	w := newWorld(t)
	// They share a tag, which the registry resolves to sha256:same: web is
	// the one running something else.
	w.opted("web", "sha256:old", "sha256:same", map[string]string{HealthLabel: "docker,uptime"})
	w.opted("db", "sha256:same", "sha256:same", nil)
	w.opted("typo", "sha256:same", "sha256:same", map[string]string{AutoupdateLabel: "registy"})
	w.docker.add(container("built", func(c *model.RunningContainer) {
		c.Spec.Image = "built:latest"
		c.Spec.Labels = map[string]string{AutoupdateLabel: "local"}
	}))
	w.docker.add(container("private", func(c *model.RunningContainer) { c.Spec.Image = "ghcr.io/me/private:1" }))

	lines, err := w.source.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"web  app:latest  [update available]  autoupdate=registry detector=conntrack health=docker,uptime idle-samples=3",
		"db  app:latest  [up to date]  autoupdate=registry detector=conntrack health=docker,port-connect,port-bound,uptime idle-samples=3",
		"typo  app:latest  [not a known autoupdate policy]",
		"built  built:latest  [no local image]  autoupdate=local detector=conntrack health=docker,port-connect,port-bound,uptime idle-samples=3",
		"private  ghcr.io/me/private:1  [registry unreachable]  autoupdate=registry detector=conntrack health=docker,port-connect,port-bound,uptime idle-samples=3",
	}
	if !slices.Equal(lines, want) {
		t.Errorf("got\n%v\nwant\n%v", lines, want)
	}
}
