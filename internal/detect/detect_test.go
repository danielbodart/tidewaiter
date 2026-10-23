package detect_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/danielbodart/gokit/clock"
	"github.com/danielbodart/tidewaiter/internal/detect"
	"github.com/danielbodart/tidewaiter/internal/fakes"
	"github.com/danielbodart/tidewaiter/internal/model"
)

var ctx = context.Background()

// table is the flows `conntrack -L` showed on a live host, as the netlink dump
// now hands them over: an ESTABLISHED and a SYN_SENT on 25565, an assured and
// an unreplied UDP flow on 19132, an ESTABLISHED on 8080 and a TIME_WAIT on
// 28080. ICMP never gets this far.
var table = []detect.ConntrackEntry{
	{Protocol: model.TCP, DestinationPort: 25565, Assured: true, State: "ESTABLISHED"},
	{Protocol: model.TCP, DestinationPort: 25565, State: "SYN_SENT"},
	{Protocol: model.UDP, DestinationPort: 19132, Assured: true},
	{Protocol: model.UDP, DestinationPort: 19132},
	{Protocol: model.TCP, DestinationPort: 8080, Assured: true, State: "ESTABLISHED"},
	{Protocol: model.TCP, DestinationPort: 28080, Assured: true, State: "TIME_WAIT"},
}

func tcp(port int) model.PublishedPort {
	return model.PublishedPort{HostPort: port, ContainerPort: port, Protocol: model.TCP}
}

func udp(port int) model.PublishedPort {
	return model.PublishedPort{HostPort: port, ContainerPort: port, Protocol: model.UDP}
}

func procNetTCP(t *testing.T) string {
	t.Helper()
	text, err := os.ReadFile("testdata/proc-net-tcp.txt")
	if err != nil {
		t.Fatal(err)
	}
	return string(text)
}

func inUse(t *testing.T, detector detect.Detector, in detect.Input) detect.Result {
	t.Helper()
	if in.Netns == nil {
		in.Netns = &fakes.Netns{}
	}
	result, err := detector.InUse(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestParseProcNetReadsLocalPortAndStateSkippingTheHeader(t *testing.T) {
	entries := detect.ParseProcNet(procNetTCP(t))
	count := func(want detect.ProcNetEntry) int {
		n := 0
		for _, entry := range entries {
			if entry == want {
				n++
			}
		}
		return n
	}
	// 1F90 is 8080 (LISTEN and two ESTABLISHED), 0016 is 22 (LISTEN).
	if count(detect.ProcNetEntry{LocalPort: 8080, State: "0A"}) != 1 ||
		count(detect.ProcNetEntry{LocalPort: 8080, State: "01"}) != 2 ||
		count(detect.ProcNetEntry{LocalPort: 22, State: "0A"}) != 1 ||
		len(entries) != 4 {
		t.Errorf("got %v", entries)
	}
}

func TestParseProcNetSkipsWhatItCannotRead(t *testing.T) {
	if got := detect.ParseProcNet("garbage\n   0: nocolon 0 0A\n   1: 0:ZZZZ 0 0A\n"); len(got) != 0 {
		t.Errorf("got %v", got)
	}
}

func TestConntrackCountsAssuredFlowsToAPublishedPortAsSessions(t *testing.T) {
	got := inUse(t, detect.Conntrack, detect.Input{Published: []model.PublishedPort{udp(19132)}, Conntrack: table})
	if want := (detect.Result{InUse: true, Sessions: 1, Counted: true, Confident: true}); got != want {
		t.Errorf("got %+v", got)
	}
}

func TestConntrackReadsAnUnassuredFlowAsInUseButUncounted(t *testing.T) {
	unassured := []detect.ConntrackEntry{{Protocol: model.TCP, DestinationPort: 25565, State: "SYN_SENT"}}
	got := inUse(t, detect.Conntrack, detect.Input{Published: []model.PublishedPort{tcp(25565)}, Conntrack: unassured})
	if want := (detect.Result{InUse: true, Confident: true}); got != want {
		t.Errorf("got %+v", got)
	}
	// Beside an ASSURED one, the ASSURED one is what is counted.
	got = inUse(t, detect.Conntrack, detect.Input{Published: []model.PublishedPort{tcp(25565)}, Conntrack: table})
	if !got.InUse || !got.Confident || got.Sessions != 1 {
		t.Errorf("got %+v", got)
	}
}

func TestConntrackReadsNoFlowsToThePortAsIdleConfidently(t *testing.T) {
	got := inUse(t, detect.Conntrack, detect.Input{Published: []model.PublishedPort{tcp(1234)}, Conntrack: table})
	if want := (detect.Result{Confident: true}); got != want {
		t.Errorf("got %+v", got)
	}
}

func TestConntrackMatchesTheProtocolAsWellAsThePort(t *testing.T) {
	got := inUse(t, detect.Conntrack, detect.Input{Published: []model.PublishedPort{tcp(19132)}, Conntrack: table})
	if got.InUse {
		t.Errorf("got %+v", got)
	}
}

// The live-host bug: port 28080 has only a TIME_WAIT flow, a finished
// connection cooling down for two minutes. It must NOT read as in use, or a
// container would look busy for minutes after its last request ended.
func TestConntrackReadsATimeWaitOnlyFlowAsIdle(t *testing.T) {
	got := inUse(t, detect.Conntrack, detect.Input{Published: []model.PublishedPort{tcp(28080)}, Conntrack: table})
	if want := (detect.Result{Confident: true}); got != want {
		t.Errorf("got %+v", got)
	}
	established := []detect.ConntrackEntry{{Protocol: model.TCP, DestinationPort: 28080, Assured: true, State: "ESTABLISHED"}}
	if got := inUse(t, detect.Conntrack, detect.Input{Published: []model.PublishedPort{tcp(28080)}, Conntrack: established}); !got.InUse {
		t.Errorf("an ESTABLISHED flow read as %+v", got)
	}
}

func TestTCPStateNamesAreConntrackToolsOwn(t *testing.T) {
	for state, want := range map[uint8]string{3: "ESTABLISHED", 7: "TIME_WAIT", 5: "CLOSE_WAIT", 1: "SYN_SENT", 200: ""} {
		if got := detect.TCPStateName(state); got != want {
			t.Errorf("%d: got %q, want %q", state, got, want)
		}
	}
}

func TestTCPCountsEstablishedSocketsOnAPublishedContainerPort(t *testing.T) {
	netns := (&fakes.Netns{}).Set(42, detect.TCP4Table, procNetTCP(t))
	got := inUse(t, detect.TCP, detect.Input{PID: 42, Published: []model.PublishedPort{tcp(8080)}, Netns: netns})
	if want := (detect.Result{InUse: true, Sessions: 2, Counted: true, Confident: true}); got != want {
		t.Errorf("got %+v", got)
	}
}

func TestTCPReadsNoEstablishedSocketsAsIdle(t *testing.T) {
	netns := (&fakes.Netns{}).Set(42, detect.TCP4Table, procNetTCP(t))
	if got := inUse(t, detect.TCP, detect.Input{PID: 42, Published: []model.PublishedPort{tcp(9999)}, Netns: netns}); got.InUse {
		t.Errorf("got %+v", got)
	}
}

func TestTCPCountsIPv6SocketsToo(t *testing.T) {
	netns := (&fakes.Netns{}).Set(42, detect.TCP6Table, fakes.ProcNetLine(8080, "01"))
	if got := inUse(t, detect.TCP, detect.Input{PID: 42, Published: []model.PublishedPort{tcp(8080)}, Netns: netns}); got.Sessions != 1 {
		t.Errorf("got %+v", got)
	}
}

func TestTCPCannotTellWithNoTCPPorts(t *testing.T) {
	got := inUse(t, detect.TCP, detect.Input{PID: 42, Published: []model.PublishedPort{udp(19132)}})
	if got != (detect.Result{}) {
		t.Errorf("got %+v", got)
	}
}

func TestTCPFailsOnAnUnreadableNetns(t *testing.T) {
	netns := &fakes.Netns{}
	netns.FailWith(os.ErrNotExist)
	if _, err := detect.TCP.InUse(ctx, detect.Input{PID: 42, Published: []model.PublishedPort{tcp(8080)}, Netns: netns}); err == nil {
		t.Error("an unreadable netns read as an answer")
	}
}

func TestNoneIsAlwaysIdleUnconfidently(t *testing.T) {
	if got := inUse(t, detect.None, detect.Input{}); got != (detect.Result{}) {
		t.Errorf("got %+v", got)
	}
}

func TestByNameResolvesTheStatelessNamesAndNothingElse(t *testing.T) {
	for name, want := range map[string]detect.Detector{"conntrack": detect.Conntrack, "tcp": detect.TCP, "none": detect.None} {
		if got, ok := detect.ByName(name); !ok || got != want || got.Name() != name {
			t.Errorf("%s: got %v", name, got)
		}
	}
	// netio is built with Netio, not resolved by name: it is stateful and
	// needs Docker and a clock.
	for _, name := range []string{"netio", "nope"} {
		if _, ok := detect.ByName(name); ok {
			t.Errorf("%s resolved", name)
		}
	}
}

// netBytes is Docker's /stats as a map a test changes between samples.
type netBytes map[string]uint64

func (b netBytes) read(_ context.Context, id string) (uint64, error) { return b[id], nil }

func netioInput(id string) detect.Input { return detect.Input{ID: id} }

func TestNetioCannotTellOnTheFirstSample(t *testing.T) {
	detector := detect.Netio(netBytes{"web": 1000}.read, clock.NewStopped(time.Unix(0, 0)), 0)
	if got := inUse(t, detector, netioInput("web")); got != detect.Busy {
		t.Errorf("got %+v", got)
	}
}

func TestNetioIsBusyWhenBytesClimbAboveTheThreshold(t *testing.T) {
	bytes, now := netBytes{"web": 0}, clock.NewStopped(time.Unix(0, 0))
	detector := detect.Netio(bytes.read, now, 100)
	inUse(t, detector, netioInput("web"))
	now.Advance(time.Second)
	bytes["web"] = 5000
	if got := inUse(t, detector, netioInput("web")); got != (detect.Result{InUse: true, Confident: true}) {
		t.Errorf("got %+v", got)
	}
}

func TestNetioIsIdleWhenBytesAreFlat(t *testing.T) {
	now := clock.NewStopped(time.Unix(0, 0))
	detector := detect.Netio(netBytes{"web": 4242}.read, now, 100)
	inUse(t, detector, netioInput("web"))
	now.Advance(time.Second)
	if got := inUse(t, detector, netioInput("web")); got != (detect.Result{Confident: true}) {
		t.Errorf("got %+v", got)
	}
}

func TestNetioKeepsASeparateBaselinePerContainer(t *testing.T) {
	bytes, now := netBytes{"a": 0, "b": 0}, clock.NewStopped(time.Unix(0, 0))
	detector := detect.Netio(bytes.read, now, 100)
	inUse(t, detector, netioInput("a"))
	inUse(t, detector, netioInput("b"))
	now.Advance(time.Second)
	bytes["a"] = 10_000
	if !inUse(t, detector, netioInput("a")).InUse || inUse(t, detector, netioInput("b")).InUse {
		t.Error("the baselines were shared")
	}
}

func TestNetioCannotTellFromTwoSamplesAtOneInstantOrACounterThatWentBack(t *testing.T) {
	bytes, now := netBytes{"web": 5000}, clock.NewStopped(time.Unix(0, 0))
	detector := detect.Netio(bytes.read, now, 100)
	inUse(t, detector, netioInput("web"))
	if got := inUse(t, detector, netioInput("web")); got != detect.Busy {
		t.Errorf("same instant: got %+v", got)
	}
	now.Advance(time.Second)
	bytes["web"] = 10
	if got := inUse(t, detector, netioInput("web")); got != detect.Busy {
		t.Errorf("recreated: got %+v", got)
	}
}

func TestNetioPassesOnAStatsFailure(t *testing.T) {
	failing := func(context.Context, string) (uint64, error) { return 0, errors.New("no such container") }
	if _, err := detect.Netio(failing, clock.System{}, 0).InUse(ctx, netioInput("web")); err == nil {
		t.Error("a failed read read as an answer")
	}
}

func TestCombine(t *testing.T) {
	idle := detect.Result{Confident: true}
	busy := detect.Result{InUse: true, Sessions: 2, Counted: true, Confident: true}

	if !detect.Combine(idle, busy).InUse {
		t.Error("in use if any detector says so")
	}
	if detect.Combine(idle, idle).InUse {
		t.Error("idle only when all agree")
	}
	if !detect.Combine(idle, idle).Confident {
		t.Error("confident when a confident detector drove the verdict")
	}
	if got := detect.Combine(detect.Busy); got != detect.Busy {
		t.Errorf("a lone cannot-tell stays unconfident, got %+v", got)
	}
	if got := detect.Combine(busy, busy); got.Sessions != 4 || !got.Counted {
		t.Errorf("sums sessions where they are countable, got %+v", got)
	}
	if got := detect.Combine(); got != (detect.Result{}) {
		t.Errorf("nothing to combine is an unconfident idle, got %+v", got)
	}
}
