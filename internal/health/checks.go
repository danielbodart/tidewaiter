package health

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/danielbodart/gokit/clock"
	"github.com/danielbodart/gokit/sockdiag"
	"github.com/danielbodart/tidewaiter/internal/detect"
	"github.com/danielbodart/tidewaiter/internal/model"
)

// BoundPorts is the local ports with a socket waiting for traffic, for one
// protocol: TCP sockets in LISTEN, UDP sockets bound and unconnected.
//
// The seam port-bound and uptime read through, so each source decides what
// "this workload's sockets" means: a container's own netns (NetnsBoundPorts),
// or for a systemd unit on the host's network the sockets whose cgroup is the
// unit's (CgroupBoundPorts) - the host netns is shared, so a port number alone
// would also see every other service's sockets. An error is "could not read
// them yet", never "nothing bound".
type BoundPorts func(ctx context.Context, protocol model.Protocol) (map[int]bool, error)

// NetnsBoundPorts is a container's bound ports, read from its netns through
// /proc/<pid>/net/*.
func NetnsBoundPorts(netns detect.Netns, pid int) BoundPorts {
	return func(_ context.Context, protocol model.Protocol) (map[int]bool, error) {
		state := detect.StateListen
		if protocol == model.UDP {
			state = detect.StateUDPBound
		}
		return detect.PortsInState(netns, pid, protocol, state)
	}
}

// CgroupBoundPorts is a unit's bound ports: the host's listening sockets
// created in its control group, or beneath it.
//
// list is asked each time, so a caller that evaluates several checks against
// one moment memoises it. A listing whose cgroups are unknown (no cgroupfs, a
// kernel before 5.7) is an error, not a listing: every socket would be
// "within nothing", which would read as a unit that bound nothing and fail
// uptime for a healthy service. So is a unit with no cgroup at all - an
// active oneshot with RemainAfterExit whose process has exited, or a poll
// that caught it between processes - for the same reason: no socket is
// within "", so it would read as a unit that bound nothing.
func CgroupBoundPorts(list func(ctx context.Context) ([]sockdiag.Listener, error), cgroup string) BoundPorts {
	return func(ctx context.Context, protocol model.Protocol) (map[int]bool, error) {
		if cgroup == "" {
			return nil, errors.New("the unit has no cgroup, so its sockets cannot be told apart")
		}
		listeners, err := list(ctx)
		if err != nil {
			return nil, err
		}
		ports := map[int]bool{}
		for _, listener := range listeners {
			if string(listener.Protocol) == string(protocol) && listener.Within(cgroup) {
				ports[listener.Port] = true
			}
		}
		return ports, nil
	}
}

// ConnectOutcome is what a single connect() to a client-facing endpoint
// concluded.
type ConnectOutcome int

const (
	// Unreachable: no host answered at all (EHOSTUNREACH, ENETUNREACH, or a
	// timeout). We cannot see the endpoint from here - the classic macvlan
	// case where the host has no route to the child interface. Never a
	// failure: "we could not look" must not veto. The zero value, so anything
	// unrecognised is fail-open.
	Unreachable ConnectOutcome = iota
	// Accepted: the handshake completed - something is listening and serving.
	Accepted
	// Refused: the path reached a host that answered with RST (ECONNREFUSED).
	// The route is wired, nothing is accepting there *yet*. On a truthful
	// endpoint this lets port-connect actively fail once the grace has passed.
	Refused
)

func (o ConnectOutcome) String() string {
	switch o {
	case Accepted:
		return "accepted"
	case Refused:
		return "refused"
	}
	return "unreachable"
}

// Prober opens a TCP connection to host:port and reports how it went.
//
// The seam for port-connect, so tests decide the answer without a real
// socket. Tidewaiter runs host-networked, so the real one connects from the
// host to the exact address a client would use. The three-way outcome (not a
// bool) is what tells "route works, nothing accepting" (a real fault) apart
// from "no route from here" (our own blind spot, never a fault).
type Prober interface {
	Connect(ctx context.Context, host string, port int) ConnectOutcome
}

// Dialer is the real Prober: connect, close at once, classify.
type Dialer struct {
	// How long a connect may take before it reads as unreachable; two seconds
	// when zero.
	Timeout time.Duration
}

func (d Dialer) Connect(ctx context.Context, host string, port int) ConnectOutcome {
	timeout := d.Timeout
	if timeout == 0 {
		timeout = 2 * time.Second
	}
	dialer := net.Dialer{Timeout: timeout}
	connection, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return ClassifyConnectError(err)
	}
	connection.Close()
	return Accepted
}

// ClassifyConnectError maps a connect() failure to an outcome.
//
// Only a clean connection refused (the route works, the peer sent RST) is an
// authoritative Refused. Everything else - no route, timeout, cancellation, or
// an error we do not recognise - is Unreachable, the outcome that can never
// fail a check, keeping the probe fail-open on anything ambiguous.
func ClassifyConnectError(err error) ConnectOutcome {
	if errors.Is(err, syscall.ECONNREFUSED) {
		return Refused
	}
	return Unreachable
}

// Endpoint is a client-facing address to probe, and whether reaching it is
// authoritative.
//
// Truthful is a direct path to the workload with no docker-proxy in front - a
// container IP on a bridge or user network, or 127.0.0.1 in host network
// mode - where Refused is a real "not serving" signal. A published host port
// is fronted by docker-proxy, which answers a connect() even with no backend:
// Accepted there is worth a pass, but Refused or Unreachable can never fail.
type Endpoint struct {
	Host     string
	Port     int
	Truthful bool
}

// ReachableDriver is whether a host-networked Tidewaiter can route to a
// container IP on a network with this driver.
//
// bridge (default and user-defined) puts an interface on the host, so the host
// routes straight to the container's IP; host shares the host stack outright.
// Everything else - macvlan and ipvlan (the host has no route to the child
// interface by default), overlay (a multi-host VXLAN the host is not on),
// none - is NOT reachable from here, so a connect result to such an IP must
// not be authoritative: a coincidental foreign RST on that subnet would
// otherwise read as the container refusing, and fail a healthy update.
func ReachableDriver(driver string) bool {
	return driver == "bridge" || driver == "host"
}

// ClientEndpoints is the TCP endpoints a client actually uses to reach these
// ports, in preference order, for each TCP port in scope:
//
//  1. TRUTHFUL container-IP endpoints - one per network the subject has a
//     runtime IP on AND whose driver is reachable, at the *container* port.
//     The host reaches that IP with no docker-proxy between, so a connect
//     there tests the exact path a client takes AND catches a service bound
//     to the wrong address (its own loopback, say) - which the netns bind
//     check, blind to listen address, waves through. An IP on a driver we
//     cannot route to is dropped, never probed.
//  2. A PROXIED fallback - 127.0.0.1 at the *host* port. docker-proxy answers
//     this even with no backend, so it can only ever pass, never fail. Kept so
//     a container behind a firewalled bridge, where the truthful endpoint is
//     momentarily unreachable, can still confirm.
//
// In host network mode there is no proxy and no per-network IP: the service
// is at 127.0.0.1 on the container port itself, a truthful loopback endpoint.
func ClientEndpoints(ctx context.Context, subject Subject, ports []model.PublishedPort, driver func(ctx context.Context, network string) string) []Endpoint {
	var tcp []model.PublishedPort
	for _, port := range ports {
		if port.Protocol == model.TCP {
			tcp = append(tcp, port)
		}
	}
	if len(tcp) == 0 {
		return nil
	}

	// Resolved once: the IPs on a driver we can actually route to.
	var reachable []string
	for _, network := range subject.Networks {
		if network.IPAddress != "" && driver != nil && ReachableDriver(driver(ctx, network.Name)) {
			reachable = append(reachable, network.IPAddress)
		}
	}

	var endpoints []Endpoint
	seen := map[string]bool{}
	add := func(host string, port int, truthful bool) {
		key := net.JoinHostPort(host, strconv.Itoa(port))
		if seen[key] {
			return
		}
		seen[key] = true
		endpoints = append(endpoints, Endpoint{Host: host, Port: port, Truthful: truthful})
	}
	hostMode := strings.HasPrefix(subject.NetworkMode, "host")
	for _, port := range tcp {
		if hostMode {
			add("127.0.0.1", port.ContainerPort, true)
		}
		for _, ip := range reachable {
			add(ip, port.ContainerPort, true)
		}
		add("127.0.0.1", port.HostPort, false)
	}
	return endpoints
}

// PortConnect is TCP only: a real connect() to the address a client actually
// uses.
//
// Built per swap, as it remembers when it first saw the subject. Each poll it
// probes every client endpoint (ClientEndpoints):
//
//   - ANY endpoint accepts: pass - something is serving, now.
//   - before `seconds` have passed: inconclusive - a startup race must never
//     read as a broken image.
//   - after that, still not passing: a TRUTHFUL endpoint refused is a fail -
//     the route is wired but nothing accepts there, bound to the wrong address
//     or not listening at all, which port-bound (blind to listen address) and
//     docker-proxy (which fakes an accept) both miss. Otherwise (only
//     unreachable, or only the proxied fallback left) inconclusive - no
//     authoritative "no", so no veto.
//
// The grace is the same `seconds` as uptime's, so a slow but real app that
// starts serving within it still passes. Skips when no TCP endpoint applies.
func PortConnect(seconds int, clock clock.Clock) Check {
	return &portConnect{seconds: seconds, clock: clock}
}

type portConnect struct {
	seconds   int
	clock     clock.Clock
	firstSeen time.Time
}

func (*portConnect) Name() string { return "port-connect" }

func (p *portConnect) Evaluate(ctx context.Context, in Input) Result {
	endpoints := ClientEndpoints(ctx, in.Subject, in.Ports, in.NetworkDriver)
	if len(endpoints) == 0 {
		return Skipped
	}
	truthfulRefused := false
	for _, endpoint := range endpoints {
		switch in.TCP.Connect(ctx, endpoint.Host, endpoint.Port) {
		case Accepted:
			return Passed
		case Refused:
			truthfulRefused = truthfulRefused || endpoint.Truthful
		}
	}
	if !p.elapsed() {
		return Waiting
	}
	if truthfulRefused {
		return Failed(fmt.Sprintf("up for %ds but refuses connections at its own address - the container is not serving clients", p.seconds))
	}
	return Waiting
}

func (p *portConnect) elapsed() bool {
	now := p.clock.Now()
	if p.firstSeen.IsZero() {
		p.firstSeen = now
	}
	return now.Sub(p.firstSeen) >= time.Duration(p.seconds)*time.Second
}

// PortBound is TCP and UDP: a listening or bound socket on a port in scope.
//
// Passes as soon as ANY port in scope has one - "at least one", so unused
// ports do not hold it back. The only generic UDP signal (a UDP send cannot
// tell open from unbound), and for TCP it usefully bypasses docker-proxy.
// Never actively fails: nothing bound yet, or sockets that cannot be read
// yet, is inconclusive. Skips when there are no ports.
var PortBound Check = portBound{}

type portBound struct{}

func (portBound) Name() string { return "port-bound" }

func (portBound) Evaluate(ctx context.Context, in Input) Result {
	if len(in.Ports) == 0 {
		return Skipped
	}
	if bound, known := anyPortBound(ctx, in); known && bound {
		return Passed
	}
	return Waiting
}

// anyPortBound is whether any port in scope has a bound socket, and whether
// that could be told at all - a netns not readable yet, the process having
// just moved, is distinct from a definite "nothing is bound".
//
// Shared by port-bound, which turns yes into a pass, and uptime, which at its
// deadline turns no into a fail: the "bound by then" rule that catches a
// container the network path accepts but nothing serves.
func anyPortBound(ctx context.Context, in Input) (bound, known bool) {
	for _, protocol := range model.Protocols {
		var wanted []int
		for _, port := range in.Ports {
			if port.Protocol == protocol {
				wanted = append(wanted, port.ContainerPort)
			}
		}
		if len(wanted) == 0 {
			continue
		}
		listening, err := in.Bound(ctx, protocol)
		if err != nil {
			return false, false
		}
		for _, port := range wanted {
			if listening[port] {
				return true, true
			}
		}
	}
	return false, true
}

// Uptime is the always-applicable check: the subject stays up, and by the
// time it has been up long enough it has bound a port, if it has any.
//
// Built per swap, as it remembers when it first saw the subject. The one check
// that couples "stayed up" with "is serving", and the only generic one that
// can catch a container the network path accepts but which never serves:
//
//   - not running (exited, crash-looping): fail.
//   - before `seconds` have passed: inconclusive.
//   - after that, still running: with no ports, pass (staying up is all we can
//     ask of a portless worker); with ports, pass once one is bound, fail if
//     none is ("up but not serving" - a real service binds within seconds),
//     and inconclusive while the sockets cannot be read.
func Uptime(seconds int, clock clock.Clock) Check {
	return &uptime{seconds: seconds, clock: clock}
}

type uptime struct {
	seconds   int
	clock     clock.Clock
	firstSeen time.Time
}

func (*uptime) Name() string { return "uptime" }

func (u *uptime) Evaluate(ctx context.Context, in Input) Result {
	if !in.Subject.Running {
		return Failed("the container exited or is restarting")
	}
	now := u.clock.Now()
	if u.firstSeen.IsZero() {
		u.firstSeen = now
	}
	if now.Sub(u.firstSeen) < time.Duration(u.seconds)*time.Second {
		return Waiting
	}
	if len(in.Ports) == 0 {
		return Passed
	}
	bound, known := anyPortBound(ctx, in)
	switch {
	case !known:
		return Waiting
	case bound:
		return Passed
	}
	return Failed(fmt.Sprintf("up for %ds but no published port is bound - the container is not serving", u.seconds))
}
