package detect

import (
	"bufio"
	"context"
	"strconv"
	"strings"

	"github.com/danielbodart/tidewaiter/internal/model"
)

// Table is one of a process's netns-scoped socket tables, /proc/<pid>/net/*.
type Table string

const (
	TCP4Table Table = "tcp"
	TCP6Table Table = "tcp6"
	UDP4Table Table = "udp"
	UDP6Table Table = "udp6"
)

// Netns reads a process's netns-scoped socket tables, host-side.
//
// Tidewaiter runs in the host PID namespace (or with the host's /proc
// mounted), so a container's main-process PID - State.Pid - is directly
// usable: Linux exposes every process's socket tables at /proc/<pid>/net/*,
// so reading them needs no setns(), no nsenter and no CAP_SYS_ADMIN. An
// interface so tests hand in canned tables keyed by pid rather than needing a
// real container. An error - the process just exited, /proc not there yet - is
// "could not tell".
type Netns interface {
	Read(pid int, table Table) (string, error)
}

// The `st` codes Linux reports in /proc/net/{tcp,udp}.
const (
	StateEstablished = "01"
	StateListen      = "0A"
	// A bound, unconnected UDP socket reports TCP_CLOSE.
	StateUDPBound = "07"
)

// ProcNetEntry is one socket from /proc/net/{tcp,udp}: its local port and
// state code.
type ProcNetEntry struct {
	LocalPort int
	State     string
}

// ParseProcNet reads /proc/net/tcp (or udp) into local port and state.
//
// Pure, and tested against captured /proc output. The format is fixed-column
// hex: the local address column is HEXIP:HEXPORT, the `st` column is the
// connection state. Only the local port and state matter here; a line that
// does not read is skipped rather than guessed at.
func ParseProcNet(text string) []ProcNetEntry {
	var entries []ProcNetEntry
	scanner := bufio.NewScanner(strings.NewReader(text))
	for scanner.Scan() {
		columns := strings.Fields(scanner.Text())
		// Columns: sl local_address rem_address st ...
		if len(columns) < 4 || columns[0] == "sl" {
			continue
		}
		colon := strings.LastIndexByte(columns[1], ':')
		if colon == -1 {
			continue
		}
		port, err := strconv.ParseUint(columns[1][colon+1:], 16, 16)
		if err != nil {
			continue
		}
		// Upper-cased so a comparison need not worry about letter case.
		entries = append(entries, ProcNetEntry{LocalPort: int(port), State: strings.ToUpper(columns[3])})
	}
	return entries
}

// PortsInState is the local ports in a given socket state, across a family's
// IPv4 and IPv6 tables.
//
// The one read the TCP detector and the port-bound health check share.
func PortsInState(netns Netns, pid int, family model.Protocol, state string) (map[int]bool, error) {
	v4, v6 := TCP4Table, TCP6Table
	if family == model.UDP {
		v4, v6 = UDP4Table, UDP6Table
	}
	ports := map[int]bool{}
	for _, table := range []Table{v4, v6} {
		text, err := netns.Read(pid, table)
		if err != nil {
			return nil, err
		}
		for _, entry := range ParseProcNet(text) {
			if entry.State == state {
				ports[entry.LocalPort] = true
			}
		}
	}
	return ports, nil
}

// TCP is the backup detector: ESTABLISHED TCP sockets in the workload's netns.
//
// TCP only - UDP has no socket-state fallback, which is exactly why conntrack
// is the primary. Reads /proc/<pid>/net/tcp[6] and counts ESTABLISHED
// connections whose local port is one the workload publishes: connections,
// not ports, so three clients on port 80 are three sessions. Confident for
// what it can see, but blind to UDP, so a supplement and never a replacement
// for conntrack on a UDP service.
var TCP Detector = tcpDetector{}

type tcpDetector struct{}

func (tcpDetector) Name() string { return "tcp" }

func (tcpDetector) InUse(_ context.Context, in Input) (Result, error) {
	ports := map[int]bool{}
	for _, port := range in.Published {
		if port.Protocol == model.TCP {
			ports[port.ContainerPort] = true
		}
	}
	if len(ports) == 0 {
		return Result{}, nil
	}
	established := 0
	for _, table := range []Table{TCP4Table, TCP6Table} {
		text, err := in.Netns.Read(in.PID, table)
		if err != nil {
			return Result{}, err
		}
		for _, entry := range ParseProcNet(text) {
			if entry.State == StateEstablished && ports[entry.LocalPort] {
				established++
			}
		}
	}
	if established > 0 {
		return Result{InUse: true, Sessions: established, Counted: true, Confident: true}, nil
	}
	return Result{Confident: true}, nil
}
