package health_test

import (
	"context"
	"errors"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/danielbodart/gokit/clock"
	"github.com/danielbodart/gokit/sockdiag"
	"github.com/danielbodart/tidewaiter/internal/detect"
	"github.com/danielbodart/tidewaiter/internal/fakes"
	"github.com/danielbodart/tidewaiter/internal/health"
	"github.com/danielbodart/tidewaiter/internal/model"
)

var ctx = context.Background()

const pid = 4242

var start = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

func port(host, container int, protocol model.Protocol) model.PublishedPort {
	return model.PublishedPort{HostPort: host, ContainerPort: container, Protocol: protocol}
}

// running is a container that is up, on no network in particular.
var running = health.Subject{Running: true, Health: model.NoHealth}

// onBridge is a container attached to a bridge network with a routable
// runtime IP.
func onBridge(ip string) health.Subject {
	subject := running
	subject.Networks = []model.NetworkAttachment{{Name: "bridge", IPAddress: ip}}
	return subject
}

// input is a health.Input with every seam defaulted: the container's own
// netns read through netns, a prober that refuses everything, and every
// network a bridge.
func input(subject health.Subject, ports []model.PublishedPort, netns *fakes.Netns, tcp *fakes.Prober) health.Input {
	if netns == nil {
		netns = &fakes.Netns{}
	}
	if tcp == nil {
		tcp = &fakes.Prober{}
	}
	return health.Input{
		Subject:       subject,
		Ports:         ports,
		Bound:         health.NetnsBoundPorts(netns, pid),
		TCP:           tcp,
		NetworkDriver: func(context.Context, string) string { return "bridge" },
	}
}

func expect(t *testing.T, got health.Result, want health.Outcome) {
	t.Helper()
	if got.Outcome != want {
		t.Errorf("got %v (%s), want %v", got.Outcome, got.Reason, want)
	}
}

func TestPortConnectPassesAsSoonAsAnyEndpointAccepts(t *testing.T) {
	tcp := (&fakes.Prober{}).Listening(18081)
	check := health.PortConnect(10, clock.NewStopped(start))
	expect(t, check.Evaluate(ctx, input(running, []model.PublishedPort{port(18080, 80, model.TCP), port(18081, 81, model.TCP)}, nil, tcp)), health.Pass)
}

func TestPortConnectWithNoContainerIPFallsBackToTheHostPort(t *testing.T) {
	tcp := (&fakes.Prober{}).Listening(18080)
	health.PortConnect(10, clock.NewStopped(start)).Evaluate(ctx, input(running, []model.PublishedPort{port(18080, 80, model.TCP)}, nil, tcp))
	if !slices.Contains(tcp.ProbedPorts(), 18080) {
		t.Errorf("probed %v", tcp.Probed())
	}
}

func TestPortConnectPrefersTheContainerIPAtTheContainerPort(t *testing.T) {
	tcp := (&fakes.Prober{}).AcceptingAt("172.17.0.2", 80)
	result := health.PortConnect(10, clock.NewStopped(start)).Evaluate(ctx, input(onBridge("172.17.0.2"), []model.PublishedPort{port(18080, 80, model.TCP)}, nil, tcp))
	expect(t, result, health.Pass)
	if tcp.Probed()[0] != (fakes.Endpoint{Host: "172.17.0.2", Port: 80}) {
		t.Errorf("probed %v", tcp.Probed())
	}
}

// The unique catch: a socket bound to the container's own loopback passes
// port-bound, blind to listen address, but is unreachable to clients.
func TestPortConnectFailsPastTheGraceWhenTheContainerIPRefuses(t *testing.T) {
	now := clock.NewStopped(start)
	tcp := (&fakes.Prober{}).RefusingAt("172.17.0.2", 80)
	check := health.PortConnect(10, now)
	in := input(onBridge("172.17.0.2"), []model.PublishedPort{port(18080, 80, model.TCP)}, nil, tcp)
	expect(t, check.Evaluate(ctx, in), health.Inconclusive)
	now.Advance(10 * time.Second)
	expect(t, check.Evaluate(ctx, in), health.Fail)
}

// A macvlan IP whose subnet is coincidentally routable to some other host
// would answer with a foreign RST. Probing it would fail a healthy update.
func TestPortConnectNeverProbesAMacvlanIP(t *testing.T) {
	now := clock.NewStopped(start)
	tcp := (&fakes.Prober{}).RefusingAt("10.0.0.5", 80)
	subject := running
	subject.Networks = []model.NetworkAttachment{{Name: "pub", IPAddress: "10.0.0.5"}}
	in := input(subject, []model.PublishedPort{port(18080, 80, model.TCP)}, nil, tcp)
	in.NetworkDriver = func(context.Context, string) string { return "macvlan" }
	check := health.PortConnect(10, now)
	check.Evaluate(ctx, in)
	now.Advance(10 * time.Second)
	expect(t, check.Evaluate(ctx, in), health.Inconclusive)
	if slices.Contains(tcp.Probed(), fakes.Endpoint{Host: "10.0.0.5", Port: 80}) {
		t.Error("probed the macvlan IP")
	}
}

func TestPortConnectSkipsOverlayAndUnresolvableNetworks(t *testing.T) {
	for _, driver := range []string{"overlay", ""} {
		tcp := (&fakes.Prober{}).RefusingAt("10.1.2.3", 80)
		subject := running
		subject.Networks = []model.NetworkAttachment{{Name: "mesh", IPAddress: "10.1.2.3"}}
		in := input(subject, []model.PublishedPort{port(80, 80, model.TCP)}, nil, tcp)
		in.NetworkDriver = func(context.Context, string) string { return driver }
		health.PortConnect(10, clock.NewStopped(start)).Evaluate(ctx, in)
		if slices.Contains(tcp.Probed(), fakes.Endpoint{Host: "10.1.2.3", Port: 80}) {
			t.Errorf("%q: probed the container IP", driver)
		}
	}
}

func TestPortConnectPassesThroughTheProxyWhenTheBridgeIsFirewalled(t *testing.T) {
	tcp := (&fakes.Prober{}).UnreachableAt("172.17.0.2", 80).Listening(18080)
	result := health.PortConnect(10, clock.NewStopped(start)).Evaluate(ctx, input(onBridge("172.17.0.2"), []model.PublishedPort{port(18080, 80, model.TCP)}, nil, tcp))
	expect(t, result, health.Pass)
}

func TestPortConnectNeverFailsOnTheProxiedPortAlone(t *testing.T) {
	now := clock.NewStopped(start)
	check := health.PortConnect(10, now)
	in := input(running, []model.PublishedPort{port(18080, 80, model.TCP)}, nil, nil)
	check.Evaluate(ctx, in)
	now.Advance(time.Minute)
	expect(t, check.Evaluate(ctx, in), health.Inconclusive)
}

func TestPortConnectInHostModeProbesLoopbackAtTheContainerPortTruthfully(t *testing.T) {
	now := clock.NewStopped(start)
	tcp := (&fakes.Prober{}).RefusingAt("127.0.0.1", 8080)
	subject := running
	subject.NetworkMode = "host"
	check := health.PortConnect(10, now)
	in := input(subject, []model.PublishedPort{port(8080, 8080, model.TCP)}, nil, tcp)
	check.Evaluate(ctx, in)
	now.Advance(10 * time.Second)
	expect(t, check.Evaluate(ctx, in), health.Fail)
}

func TestPortConnectSkipsWithNoTCPPorts(t *testing.T) {
	expect(t, health.PortConnect(10, clock.NewStopped(start)).Evaluate(ctx, input(running, []model.PublishedPort{port(19132, 19132, model.UDP)}, nil, nil)), health.Skip)
}

func TestPortBoundPassesWhenABoundUDPSocketIsPresent(t *testing.T) {
	netns := (&fakes.Netns{}).Set(pid, detect.UDP4Table, "  sl  local_address\n"+fakes.ProcNetLine(19132, "07"))
	expect(t, health.PortBound.Evaluate(ctx, input(running, []model.PublishedPort{port(19132, 19132, model.UDP)}, netns, nil)), health.Pass)
}

func TestPortBoundPassesWhenAnyOfSeveralPortsIsBound(t *testing.T) {
	netns := (&fakes.Netns{}).Set(pid, detect.TCP6Table, fakes.ProcNetLine(8081, "0A"))
	ports := []model.PublishedPort{port(28080, 8080, model.TCP), port(28081, 8081, model.TCP)}
	expect(t, health.PortBound.Evaluate(ctx, input(running, ports, netns, nil)), health.Pass)
}

func TestPortBoundIsInconclusiveWithNothingBoundYetOrAnUnreadableNetns(t *testing.T) {
	ports := []model.PublishedPort{port(19132, 19132, model.UDP)}
	expect(t, health.PortBound.Evaluate(ctx, input(running, ports, (&fakes.Netns{}).Set(pid, detect.UDP4Table, "  sl\n"), nil)), health.Inconclusive)
	unreadable := &fakes.Netns{}
	unreadable.FailWith(errors.New("open /proc/999/net/udp: no such file or directory"))
	expect(t, health.PortBound.Evaluate(ctx, input(running, ports, unreadable, nil)), health.Inconclusive)
}

func TestPortBoundSkipsWithNoPorts(t *testing.T) {
	expect(t, health.PortBound.Evaluate(ctx, input(running, nil, nil, nil)), health.Skip)
}

func TestUptimeIsInconclusiveUntilTheSecondsPassThenPasses(t *testing.T) {
	now := clock.NewStopped(start)
	check := health.Uptime(10, now)
	in := input(running, nil, nil, nil)
	expect(t, check.Evaluate(ctx, in), health.Inconclusive)
	now.Advance(5 * time.Second)
	expect(t, check.Evaluate(ctx, in), health.Inconclusive)
	now.Advance(5 * time.Second)
	expect(t, check.Evaluate(ctx, in), health.Pass)
}

func TestUptimeFailsAtOnceForAContainerThatExited(t *testing.T) {
	expect(t, health.Uptime(10, clock.NewStopped(start)).Evaluate(ctx, input(health.Subject{}, nil, nil, nil)), health.Fail)
}

func TestUptimeAtItsDeadlineWantsAPortBound(t *testing.T) {
	ports := []model.PublishedPort{port(8080, 8080, model.TCP)}
	listening := (&fakes.Netns{}).Set(pid, detect.TCP4Table, fakes.ProcNetLine(8080, "0A"))
	nothing := (&fakes.Netns{}).Set(pid, detect.TCP4Table, "  sl\n")
	unreadable := &fakes.Netns{}
	unreadable.FailWith(errors.New("ENOENT"))

	for _, c := range []struct {
		name  string
		netns *fakes.Netns
		ports []model.PublishedPort
		after time.Duration
		want  health.Outcome
	}{
		{"bound by the deadline", listening, ports, 10 * time.Second, health.Pass},
		{"nothing bound by the deadline: up but not serving", nothing, ports, 10 * time.Second, health.Fail},
		{"unbound before the deadline: given time", nothing, ports, 5 * time.Second, health.Inconclusive},
		{"portless: on time alone", nothing, nil, 10 * time.Second, health.Pass},
		{"unreadable at the deadline: not a fail", unreadable, ports, 10 * time.Second, health.Inconclusive},
	} {
		t.Run(c.name, func(t *testing.T) {
			now := clock.NewStopped(start)
			check := health.Uptime(10, now)
			in := input(running, c.ports, c.netns, nil)
			check.Evaluate(ctx, in)
			now.Advance(c.after)
			expect(t, check.Evaluate(ctx, in), c.want)
		})
	}
}

func TestCgroupBoundPortsAreTheUnitsOwnSockets(t *testing.T) {
	listing := func(context.Context) ([]sockdiag.Listener, error) {
		return []sockdiag.Listener{
			{Protocol: sockdiag.UDP, Port: 19132, Cgroup: "/system.slice/bedrock.service"},
			{Protocol: sockdiag.TCP, Port: 22, Cgroup: "/system.slice/sshd.service"},
			{Protocol: sockdiag.TCP, Port: 8080, Cgroup: "/system.slice/bedrock.service/worker"},
		}, nil
	}
	bound := health.CgroupBoundPorts(listing, "/system.slice/bedrock.service")
	tcp, _ := bound(ctx, model.TCP)
	udp, _ := bound(ctx, model.UDP)
	if len(tcp) != 1 || !tcp[8080] || len(udp) != 1 || !udp[19132] {
		t.Errorf("tcp %v, udp %v", tcp, udp)
	}
}

// Without cgroupfs every socket is "within nothing": a listing that would
// read as a unit that bound nothing, and fail uptime for a healthy service.
func TestCgroupBoundPortsCannotTellWhenTheCgroupsAreUnknown(t *testing.T) {
	listing := func(context.Context) ([]sockdiag.Listener, error) {
		return []sockdiag.Listener{{Protocol: sockdiag.TCP, Port: 8080}}, sockdiag.ErrUnknownCgroups
	}
	if _, err := health.CgroupBoundPorts(listing, "/system.slice/a.service")(ctx, model.TCP); err == nil {
		t.Error("an unknown-cgroups listing read as an answer")
	}
}

// An active unit need not have a cgroup - a RemainAfterExit oneshot whose
// process has exited has none - and no socket is within "": cannot tell,
// rather than nothing bound, so neither check fails a healthy update.
func TestCgroupBoundPortsCannotTellWhenTheUnitHasNoCgroup(t *testing.T) {
	listing := func(context.Context) ([]sockdiag.Listener, error) {
		return []sockdiag.Listener{{Protocol: sockdiag.TCP, Port: 8080, Cgroup: "/system.slice/x.service"}}, nil
	}
	bound := health.CgroupBoundPorts(listing, "")
	if ports, err := bound(ctx, model.TCP); err == nil {
		t.Errorf("read as an answer: %v", ports)
	}

	in := input(running, []model.PublishedPort{port(8080, 8080, model.TCP)}, nil, nil)
	in.Bound = bound
	now := clock.NewStopped(start)
	uptime := health.Uptime(10, now)
	uptime.Evaluate(ctx, in)
	now.Advance(10 * time.Second)
	expect(t, uptime.Evaluate(ctx, in), health.Inconclusive)
	expect(t, health.PortBound.Evaluate(ctx, in), health.Inconclusive)
}

func TestCgroupBoundPortsAreNotASiblingsSharingThePrefix(t *testing.T) {
	listing := func(context.Context) ([]sockdiag.Listener, error) {
		return []sockdiag.Listener{{Protocol: sockdiag.TCP, Port: 8080, Cgroup: "/system.slice/bedrock.service2"}}, nil
	}
	if ports, err := health.CgroupBoundPorts(listing, "/system.slice/bedrock.service")(ctx, model.TCP); err != nil || len(ports) != 0 {
		t.Errorf("got %v, %v", ports, err)
	}
}

func r(outcome health.Outcome) health.Result { return health.Result{Outcome: outcome} }

func TestCombine(t *testing.T) {
	for _, c := range []struct {
		name     string
		results  []health.Result
		timedOut bool
		settled  bool
		healthy  bool
	}{
		{"any fail rolls back, even beside passes", []health.Result{r(health.Pass), r(health.Fail)}, false, true, false},
		{"every applicable check passed commits", []health.Result{r(health.Pass), r(health.Skip)}, false, true, true},
		{"some inconclusive, none failed, keeps polling", []health.Result{r(health.Pass), r(health.Inconclusive)}, false, false, false},
		{"at the timeout inconclusive does not veto", []health.Result{r(health.Pass), r(health.Inconclusive)}, true, true, true},
		{"at the timeout all inconclusive commits", []health.Result{r(health.Inconclusive), r(health.Inconclusive)}, true, true, true},
		{"at the timeout a fail still rolls back", []health.Result{r(health.Inconclusive), r(health.Fail)}, true, true, false},
		{"all skipped settles to commit rather than hanging", []health.Result{r(health.Skip), r(health.Skip)}, false, true, true},
	} {
		verdict, settled := health.Combine(c.results, c.timedOut)
		if settled != c.settled || (settled && verdict.Healthy != c.healthy) {
			t.Errorf("%s: got %+v settled=%v", c.name, verdict, settled)
		}
		if settled && !verdict.Healthy && verdict.Reason == "" {
			t.Errorf("%s: a rollback with no reason", c.name)
		}
	}
	if verdict, _ := health.Combine([]health.Result{health.Failed("it exited")}, false); verdict.Reason != "it exited" {
		t.Errorf("the rollback's reason is the check's, got %q", verdict.Reason)
	}
}

func TestUptimeSecondsIsTwoThirdsOfTheTimeoutWithinTwoToThirty(t *testing.T) {
	for timeout, want := range map[int]int{1: 2, 3: 2, 10: 6, 45: 29, 60: 30, 120: 30} {
		if got := health.UptimeSeconds(timeout); got != want {
			t.Errorf("%d: got %d, want %d", timeout, got, want)
		}
	}
}

// gate runs Gate on its own goroutine, so a test can move the stopped clock
// on while it waits.
func gate(c context.Context, now *clock.Stopped, evaluate func(context.Context) ([]health.Result, error)) chan struct {
	health.Verdict
	error
} {
	done := make(chan struct {
		health.Verdict
		error
	}, 1)
	go func() {
		verdict, err := health.Gate(c, now, health.Timing{Timeout: 10 * time.Second, Poll: 2 * time.Second}, evaluate)
		done <- struct {
			health.Verdict
			error
		}{verdict, err}
	}()
	return done
}

func TestGateCommitsOnceEveryCheckPasses(t *testing.T) {
	now := clock.NewStopped(start)
	polls := 0
	done := gate(ctx, now, func(context.Context) ([]health.Result, error) {
		polls++
		if polls < 3 {
			return []health.Result{health.Waiting}, nil
		}
		return []health.Result{health.Passed}, nil
	})
	for range 2 {
		if err := now.BlockUntil(ctx, 1); err != nil {
			t.Fatal(err)
		}
		now.Advance(2 * time.Second)
	}
	got := <-done
	if got.error != nil || !got.Healthy || polls != 3 {
		t.Errorf("got %+v after %d polls", got, polls)
	}
}

func TestGateRollsBackOnAFailureOrAnError(t *testing.T) {
	now := clock.NewStopped(start)
	got := <-gate(ctx, now, func(context.Context) ([]health.Result, error) {
		return []health.Result{health.Passed, health.Failed("exited")}, nil
	})
	if got.Healthy || got.Reason != "exited" {
		t.Errorf("got %+v", got)
	}
	got = <-gate(ctx, now, func(context.Context) ([]health.Result, error) { return nil, errors.New("no such container") })
	if got.Healthy || got.Reason != "health check errored: no such container" {
		t.Errorf("got %+v", got)
	}
}

func TestGateCommitsAtTheDeadlineUnlessSomethingFailed(t *testing.T) {
	now := clock.NewStopped(start)
	done := gate(ctx, now, func(context.Context) ([]health.Result, error) { return []health.Result{health.Waiting}, nil })
	for range 5 {
		if err := now.BlockUntil(ctx, 1); err != nil {
			t.Fatal(err)
		}
		now.Advance(2 * time.Second)
	}
	if got := <-done; got.error != nil || !got.Healthy {
		t.Errorf("got %+v", got)
	}
}

func TestGateGivesNoVerdictWhenItsContextIsDone(t *testing.T) {
	c, cancel := context.WithCancel(ctx)
	now := clock.NewStopped(start)
	done := gate(c, now, func(context.Context) ([]health.Result, error) { return []health.Result{health.Waiting}, nil })
	if err := now.BlockUntil(ctx, 1); err != nil {
		t.Fatal(err)
	}
	cancel()
	if got := <-done; !errors.Is(got.error, context.Canceled) {
		t.Errorf("got %+v", got)
	}
}

func TestDialerTellsAcceptedFromRefused(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback to listen on:", err)
	}
	address := listener.Addr().(*net.TCPAddr)
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			connection.Close()
		}
	}()
	if got := (health.Dialer{}).Connect(ctx, "127.0.0.1", address.Port); got != health.Accepted {
		t.Errorf("listening: got %v", got)
	}
	listener.Close()
	if got := (health.Dialer{}).Connect(ctx, "127.0.0.1", address.Port); got != health.Refused {
		t.Errorf("closed: got %v", got)
	}
}

func TestOnlyARefusalIsRefused(t *testing.T) {
	if got := health.ClassifyConnectError(errors.New("i/o timeout")); got != health.Unreachable {
		t.Errorf("got %v", got)
	}
	if got := health.ClassifyConnectError(context.Canceled); got != health.Unreachable {
		t.Errorf("got %v", got)
	}
}
