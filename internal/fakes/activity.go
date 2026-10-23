package fakes

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/danielbodart/tidewaiter/internal/detect"
	"github.com/danielbodart/tidewaiter/internal/health"
	"github.com/danielbodart/tidewaiter/internal/model"
)

// Conntrack is the host conntrack table, as entries a test sets.
//
// Calls counts dumps, so a test can prove the daemon dumps once per pass
// rather than once per workload. FailWith exercises the fail-safe path.
type Conntrack struct {
	mu      sync.Mutex
	entries []detect.ConntrackEntry
	err     error
	calls   int
}

// Flows replaces the table.
func (c *Conntrack) Flows(entries ...detect.ConntrackEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = entries
}

// FailWith makes Dump fail with err, until it is called with nil.
func (c *Conntrack) FailWith(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
}

// Calls is how many dumps there have been.
func (c *Conntrack) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func (c *Conntrack) Dump(context.Context) ([]detect.ConntrackEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	return slices.Clone(c.entries), nil
}

// Flow is an ESTABLISHED, assured TCP flow to port - a client on it. UDP
// flows, or other states, are written out as detect.ConntrackEntry.
func Flow(port int) detect.ConntrackEntry {
	return detect.ConntrackEntry{Protocol: model.TCP, DestinationPort: port, Assured: true, State: "ESTABLISHED"}
}

// Netns is /proc/<pid>/net/{tcp,tcp6,udp,udp6} as text a test sets, by pid.
// A table never set reads as empty.
type Netns struct {
	mu     sync.Mutex
	tables map[string]string
	err    error
}

// Set is the text one of a pid's tables reads as.
func (n *Netns) Set(pid int, table detect.Table, text string) *Netns {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.tables == nil {
		n.tables = map[string]string{}
	}
	n.tables[fmt.Sprintf("%d:%s", pid, table)] = text
	return n
}

// FailWith makes every read fail with err, as the real reader does when
// /proc/<pid>/net/* is not there yet - a freshly started or just exited
// process - until it is called with nil.
func (n *Netns) FailWith(err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.err = err
}

func (n *Netns) Read(pid int, table detect.Table) (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.err != nil {
		return "", n.err
	}
	return n.tables[fmt.Sprintf("%d:%s", pid, table)], nil
}

// ProcNetLine is one /proc/net/{tcp,udp} socket line on port in state ("0A"
// listening, "01" established, "07" a bound UDP socket), to Set a table from.
func ProcNetLine(port int, state string) string {
	return fmt.Sprintf("   0: 00000000:%04X 00000000:0000 %s 00000000:00000000 00:00000000 00000000     0        0 1 1 0000000000000000 100 0 0 10 0\n", port, state)
}

// Endpoint is a host and port a Prober was asked about.
type Endpoint struct {
	Host string
	Port int
}

// Prober is the TCP connect probe, answering as a test declares.
//
// The ergonomic default matches how most tests think - "these ports are up",
// on any host, via Listening - while AcceptingAt, RefusingAt and
// UnreachableAt pin a host:port when a test cares about the exact address
// (bridge IP versus loopback). Anything not declared is refused: a wired
// route with nothing serving, the common "not up yet" case.
type Prober struct {
	mu      sync.Mutex
	answers map[Endpoint]health.ConnectOutcome
	open    map[int]bool
	probed  []Endpoint
}

// Listening makes these ports accept on any host.
func (p *Prober) Listening(ports ...int) *Prober {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.open == nil {
		p.open = map[int]bool{}
	}
	for _, port := range ports {
		p.open[port] = true
	}
	return p
}

func (p *Prober) AcceptingAt(host string, port int) *Prober {
	return p.answer(host, port, health.Accepted)
}

func (p *Prober) RefusingAt(host string, port int) *Prober {
	return p.answer(host, port, health.Refused)
}

func (p *Prober) UnreachableAt(host string, port int) *Prober {
	return p.answer(host, port, health.Unreachable)
}

func (p *Prober) answer(host string, port int, outcome health.ConnectOutcome) *Prober {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.answers == nil {
		p.answers = map[Endpoint]health.ConnectOutcome{}
	}
	p.answers[Endpoint{host, port}] = outcome
	return p
}

// Probed is every endpoint asked about, in order.
func (p *Prober) Probed() []Endpoint {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.probed)
}

// ProbedPorts is every port asked about, whatever the host.
func (p *Prober) ProbedPorts() []int {
	var ports []int
	for _, endpoint := range p.Probed() {
		ports = append(ports, endpoint.Port)
	}
	return ports
}

func (p *Prober) Connect(_ context.Context, host string, port int) health.ConnectOutcome {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.probed = append(p.probed, Endpoint{host, port})
	if outcome, ok := p.answers[Endpoint{host, port}]; ok {
		return outcome
	}
	if p.open[port] {
		return health.Accepted
	}
	return health.Refused
}
