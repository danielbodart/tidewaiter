package detect

import (
	"context"

	"github.com/danielbodart/gokit/conntrack"
	"github.com/danielbodart/tidewaiter/internal/model"
)

// ConntrackEntry is one flow from the host's conntrack table, reduced to what
// a detector needs.
//
// DestinationPort is the original-direction destination port - the host port
// a client connected to, which is what a container's published host ports are
// matched against. Assured marks a flow conntrack has seen traffic in both
// directions on (IPS_ASSURED), which separates a real session from a lone
// probe or a SYN that went nowhere.
//
// State is the TCP connection state, by the names conntrack-tools prints
// (ESTABLISHED, TIME_WAIT, ... - conntrack.TCPState's), and empty for UDP, which
// conntrack tracks with no such state. It is load-bearing: a TIME_WAIT or
// CLOSE_WAIT flow is a connection that has *finished*, and it lingers in the
// table for up to two minutes. Counting those as in use would make a
// container read busy for two minutes after its last request ended.
type ConntrackEntry struct {
	Protocol        model.Protocol
	DestinationPort int
	Assured         bool
	State           string
}

// ConntrackDumper reads the host's conntrack table, every family, TCP and UDP
// flows only.
//
// An interface, so the daemon dumps once per pass and hands the same table to
// every source's detectors, and so tests feed entries with no netlink and no
// CAP_NET_ADMIN. The whole table comes back host-wide - NAT and routing
// happen at the host whichever netns the destination lives in - so one dump
// serves every workload, filtered in memory by port. An error is "could not
// tell", never an empty table.
type ConntrackDumper interface {
	Dump(ctx context.Context) ([]ConntrackEntry, error)
}

// ConntrackDumperFunc is a func as a ConntrackDumper.
type ConntrackDumperFunc func(ctx context.Context) ([]ConntrackEntry, error)

func (f ConntrackDumperFunc) Dump(ctx context.Context) ([]ConntrackEntry, error) { return f(ctx) }

// FromConntrack is a ConntrackDumper over the whole table dumper reads -
// conntrack.Kernel(), in the daemon - each dump reduced by Reduce. An error
// from it is returned as it is, never as an empty table.
func FromConntrack(dumper conntrack.Dumper) ConntrackDumper {
	return ConntrackDumperFunc(func(ctx context.Context) ([]ConntrackEntry, error) {
		entries, err := dumper.Dump(ctx)
		if err != nil {
			return nil, err
		}
		return Reduce(entries), nil
	})
}

// Reduce is the flows of a conntrack table a detector counts: TCP and UDP,
// the protocols the gate is scoped to, with a destination port to match.
//
// The destination port is the original direction's - the host port the
// client aimed at - not the reply's, which NAT may have rewritten to the
// container's own. Never nil, so an empty table stays "no flows" rather than
// "could not tell".
func Reduce(entries []conntrack.Entry) []ConntrackEntry {
	reduced := []ConntrackEntry{}
	for _, entry := range entries {
		var protocol model.Protocol
		switch entry.Protocol {
		case conntrack.TCP:
			protocol = model.TCP
		case conntrack.UDP:
			protocol = model.UDP
		default:
			continue
		}
		if entry.Original.DestinationPort == 0 {
			continue
		}
		flow := ConntrackEntry{
			Protocol:        protocol,
			DestinationPort: entry.Original.DestinationPort,
			Assured:         entry.Status.Has(conntrack.Assured),
		}
		if protocol == model.TCP {
			flow.State = entry.TCPState.String()
		}
		reduced = append(reduced, flow)
	}
	return reduced
}

// closingTCPStates are TCP states that mean the connection is closing or
// closed.
//
// A conntrack entry in one of these is a *finished* connection cooling down -
// TIME_WAIT alone lingers about two minutes. Counting it as in use would keep
// a container busy long after its last request ended, so the detector drops
// these and counts only genuinely-active TCP flows. UDP has no state, so UDP
// flows are never excluded here (their liveness is judged by Assured instead).
var closingTCPStates = map[string]bool{
	"TIME_WAIT": true, "CLOSE_WAIT": true, "CLOSE": true, "CLOSING": true, "FIN_WAIT": true, "LAST_ACK": true,
}

// active is whether a flow is a live session, not a closing or closed TCP
// connection.
func active(entry ConntrackEntry) bool {
	return entry.State == "" || !closingTCPStates[entry.State]
}

// Conntrack is the primary detector: live flows from the host conntrack table.
//
// The only thing that sees UDP peers at all, since a UDP server has no
// per-client socket to count. Counts flows whose destination is one of the
// workload's published host ports, preferring ASSURED ones - a flow conntrack
// has seen both directions on is a real session, where a lone SYN or a probe
// packet is not. Presence is confident; ASSURED flows are counted as
// sessions, unassured ones still mark in use but are not counted.
var Conntrack Detector = conntrackDetector{}

type conntrackDetector struct{}

func (conntrackDetector) Name() string { return "conntrack" }

func (conntrackDetector) InUse(_ context.Context, in Input) (Result, error) {
	type key struct {
		protocol model.Protocol
		port     int
	}
	ports := map[key]bool{}
	for _, port := range in.Published {
		ports[key{port.Protocol, port.HostPort}] = true
	}
	flows, assured := 0, 0
	for _, entry := range in.Conntrack {
		if !ports[key{entry.Protocol, entry.DestinationPort}] || !active(entry) {
			continue
		}
		flows++
		if entry.Assured {
			assured++
		}
	}
	switch {
	case assured > 0:
		return Result{InUse: true, Sessions: assured, Counted: true, Confident: true}, nil
	case flows > 0:
		// Flows exist but none is ASSURED yet - in use (conservative), but
		// with no session count we could stand behind.
		return Result{InUse: true, Confident: true}, nil
	}
	return Result{Confident: true}, nil
}
