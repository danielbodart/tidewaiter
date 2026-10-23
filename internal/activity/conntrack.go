// Package activity holds the kernel-facing readers the activity detectors and
// health checks are fed from: the host's conntrack table, over netlink, and a
// process's socket tables, through /proc. The detectors themselves, and what
// they make of these, are in internal/detect.
package activity

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"syscall"
	"time"

	"github.com/danielbodart/tidewaiter/internal/detect"
	"github.com/danielbodart/tidewaiter/internal/model"
)

// Conntrack is the host's conntrack table, asked of the kernel directly over
// NETLINK_NETFILTER - the ctnetlink interface `conntrack -L` itself uses - so
// nothing has to be installed, run or parsed as text, which is what the
// TypeScript did.
//
// One dump per address family, IPv4 then IPv6, each the whole table for the
// network namespace Tidewaiter runs in: host-networked, that is the host's,
// where NAT and routing happen whichever netns a flow ends up in. Only TCP and
// UDP flows are kept, since the gate is scoped to those two.
//
// The kernel asks for CAP_NET_ADMIN to read the table, and without it says
// EPERM. Every error is returned as such, never as an empty table: the
// daemon reads it as "could not tell", which the gate reads as busy.
func Conntrack() detect.ConntrackDumper { return conntrack{timeout: dumpTimeout} }

// dumpTimeout bounds a dump that the context does not: the daemon's pass runs
// on a context that is not cancelled on shutdown, and a kernel that stopped
// answering mid-dump must not stall it. A dump of even a busy table takes
// milliseconds.
const dumpTimeout = 10 * time.Second

type conntrack struct{ timeout time.Duration }

// From linux/netlink.h, linux/netfilter/nfnetlink.h,
// linux/netfilter/nfnetlink_conntrack.h and
// linux/netfilter/nf_conntrack_common.h; the syscall package has the netlink
// basics but none of netfilter.
const (
	netlinkNetfilter = 12

	nfnlSubsysCTNetlink = 1
	ipctnlMsgCTNew      = 0
	ipctnlMsgCTGet      = 1

	// struct nfgenmsg: family, version, resource id - before every
	// ctnetlink message's attributes.
	nfgenmsgLength = 4

	ctaTupleOrig = 1
	ctaStatus    = 3
	ctaProtoinfo = 4

	ctaTupleProto = 2

	ctaProtoNum     = 1
	ctaProtoDstPort = 3

	ctaProtoinfoTCP      = 1
	ctaProtoinfoTCPState = 1

	// IPS_ASSURED_BIT: conntrack has seen traffic both ways.
	ipsAssured = 1 << 2

	// The attribute type without NLA_F_NESTED and NLA_F_NET_BYTEORDER, which
	// the kernel sets on nested attributes and some readers check for.
	nlaTypeMask = 0x3fff
)

// families is what is dumped, one request each: the kernel answers a
// ctnetlink dump for the family it is asked about.
var families = []uint8{syscall.AF_INET, syscall.AF_INET6}

func (c conntrack) Dump(ctx context.Context) ([]detect.ConntrackEntry, error) {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, netlinkNetfilter)
	if err != nil {
		return nil, fmt.Errorf("opening conntrack netlink socket: %w", err)
	}
	defer syscall.Close(fd)
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return nil, fmt.Errorf("binding conntrack netlink socket: %w", err)
	}
	timeout := c.timeout
	if deadline, ok := ctx.Deadline(); ok {
		timeout = min(timeout, time.Until(deadline))
	}
	if timeout <= 0 {
		return nil, context.DeadlineExceeded
	}
	timeval := syscall.NsecToTimeval(timeout.Nanoseconds())
	if err := syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &timeval); err != nil {
		return nil, fmt.Errorf("setting conntrack netlink timeout: %w", err)
	}

	// Non-nil from the start: an empty table is "no flows", which is not the
	// same thing as the nil of "could not tell".
	entries := []detect.ConntrackEntry{}
	for i, family := range families {
		seq := uint32(i + 1)
		if err := syscall.Sendto(fd, request(seq, family), 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
			return nil, fmt.Errorf("asking for the conntrack table: %w", err)
		}
		found, err := collect(ctx, receiver(fd), seq)
		if err != nil {
			return nil, fmt.Errorf("reading the %s conntrack table: %w", familyName(family), err)
		}
		entries = append(entries, found...)
	}
	return entries, nil
}

func familyName(family uint8) string {
	if family == syscall.AF_INET6 {
		return "IPv6"
	}
	return "IPv4"
}

// request is an IPCTNL_MSG_CT_GET dump request for every flow of one family.
func request(seq uint32, family uint8) []byte {
	message := make([]byte, syscall.NLMSG_HDRLEN+nfgenmsgLength)
	order := binary.NativeEndian
	order.PutUint32(message[0:], uint32(len(message)))
	order.PutUint16(message[4:], nfnlSubsysCTNetlink<<8|ipctnlMsgCTGet)
	order.PutUint16(message[6:], syscall.NLM_F_REQUEST|syscall.NLM_F_DUMP)
	order.PutUint32(message[8:], seq)
	// nfgenmsg: the family, NFNETLINK_V0, and a resource id of 0.
	message[syscall.NLMSG_HDRLEN] = family
	return message
}

// receiver reads one datagram at a time off the socket - a dump's reply comes
// in as many as the table needs.
func receiver(fd int) func() ([]byte, error) {
	// Twice the most the kernel puts in one datagram of a dump (32KiB), so a
	// datagram always fits whole.
	buffer := make([]byte, 64*1024)
	return func() ([]byte, error) {
		for {
			n, _, flags, _, err := syscall.Recvmsg(fd, buffer, nil, 0)
			switch {
			case errors.Is(err, syscall.EINTR):
				continue
			case errors.Is(err, syscall.EAGAIN):
				return nil, fmt.Errorf("the kernel stopped answering: %w", context.DeadlineExceeded)
			case err != nil:
				return nil, err
			case flags&syscall.MSG_TRUNC != 0:
				// Half a datagram would lose flows silently, and fewer flows
				// read as idler than the table is.
				return nil, errors.New("a conntrack datagram was truncated")
			}
			return buffer[:n], nil
		}
	}
}

// collect reads one dump's reply, datagram by datagram, until the kernel says
// it is done or that it failed.
//
// Separate from the socket, so it is tested against messages built in the
// test: the parsing is where the interesting mistakes live.
func collect(ctx context.Context, receive func() ([]byte, error), seq uint32) ([]detect.ConntrackEntry, error) {
	entries := []detect.ConntrackEntry{}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		datagram, err := receive()
		if err != nil {
			return nil, err
		}
		messages, err := syscall.ParseNetlinkMessage(datagram)
		if err != nil {
			return nil, err
		}
		for _, message := range messages {
			if message.Header.Seq != seq {
				// A late answer to a request before this one.
				continue
			}
			switch message.Header.Type {
			case syscall.NLMSG_DONE, syscall.NLMSG_ERROR:
				// Both carry an errno, negated; DONE's is 0 unless the dump
				// failed part way, and an ERROR of 0 is an acknowledgement.
				if len(message.Data) >= 4 {
					if errno := int32(binary.NativeEndian.Uint32(message.Data)); errno < 0 {
						return nil, syscall.Errno(-errno)
					}
				}
				if message.Header.Type == syscall.NLMSG_DONE {
					return entries, nil
				}
			case nfnlSubsysCTNetlink<<8 | ipctnlMsgCTGet, nfnlSubsysCTNetlink<<8 | ipctnlMsgCTNew:
				// Current kernels answer a GET as GET, older ones as NEW.
				entry, ok, err := parseFlow(message.Data)
				if err != nil {
					return nil, err
				}
				if ok {
					entries = append(entries, entry)
				}
			}
		}
	}
}

// parseFlow reads one ctnetlink flow into an entry, or says it is not one a
// detector counts: not TCP or UDP, or with no destination port to match.
//
// The destination port is the original direction's - the host port the
// client aimed at - not the reply's, which is the client's own. A flow whose
// attributes do not hold together is an error rather than skipped: the dump
// as a whole is then not to be trusted.
func parseFlow(data []byte) (detect.ConntrackEntry, bool, error) {
	if len(data) < nfgenmsgLength {
		return detect.ConntrackEntry{}, false, fmt.Errorf("short ctnetlink message: %d bytes", len(data))
	}
	top, err := attributes(data[nfgenmsgLength:])
	if err != nil {
		return detect.ConntrackEntry{}, false, err
	}
	tuple, err := attributes(top[ctaTupleOrig])
	if err != nil {
		return detect.ConntrackEntry{}, false, err
	}
	proto, err := attributes(tuple[ctaTupleProto])
	if err != nil {
		return detect.ConntrackEntry{}, false, err
	}

	var entry detect.ConntrackEntry
	number := proto[ctaProtoNum]
	switch {
	case len(number) < 1:
		return detect.ConntrackEntry{}, false, nil
	case number[0] == syscall.IPPROTO_TCP:
		entry.Protocol = model.TCP
	case number[0] == syscall.IPPROTO_UDP:
		entry.Protocol = model.UDP
	default:
		return detect.ConntrackEntry{}, false, nil
	}
	port := proto[ctaProtoDstPort]
	if len(port) < 2 {
		return detect.ConntrackEntry{}, false, nil
	}
	// Ports and the status are in network order.
	entry.DestinationPort = int(binary.BigEndian.Uint16(port))
	if entry.DestinationPort == 0 {
		return detect.ConntrackEntry{}, false, nil
	}
	if status := top[ctaStatus]; len(status) >= 4 {
		entry.Assured = binary.BigEndian.Uint32(status)&ipsAssured != 0
	}

	if entry.Protocol == model.TCP {
		info, err := attributes(top[ctaProtoinfo])
		if err != nil {
			return detect.ConntrackEntry{}, false, err
		}
		tcp, err := attributes(info[ctaProtoinfoTCP])
		if err != nil {
			return detect.ConntrackEntry{}, false, err
		}
		// No state at all is left empty, as UDP's is: counted as live, the
		// cautious way.
		if state := tcp[ctaProtoinfoTCPState]; len(state) >= 1 {
			entry.State = detect.TCPStateName(state[0])
		}
	}
	return entry, true, nil
}

// attributes reads a run of netlink attributes into their payloads by type.
// An empty run is no attributes, so an absent nest reads as an empty one.
func attributes(data []byte) (map[uint16][]byte, error) {
	found := map[uint16][]byte{}
	order := binary.NativeEndian
	for len(data) > 0 {
		if len(data) < syscall.SizeofRtAttr {
			return nil, fmt.Errorf("malformed ctnetlink attribute: %d stray bytes", len(data))
		}
		length := int(order.Uint16(data[0:2]))
		if length < syscall.SizeofRtAttr || length > len(data) {
			return nil, fmt.Errorf("malformed ctnetlink attribute: length %d of %d bytes", length, len(data))
		}
		found[order.Uint16(data[2:4])&nlaTypeMask] = data[syscall.SizeofRtAttr:length]
		aligned := (length + syscall.RTA_ALIGNTO - 1) &^ (syscall.RTA_ALIGNTO - 1)
		data = data[min(aligned, len(data)):]
	}
	return found, nil
}
