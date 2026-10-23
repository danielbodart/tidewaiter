package activity

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"reflect"
	"slices"
	"syscall"
	"testing"

	"github.com/danielbodart/tidewaiter/internal/detect"
	"github.com/danielbodart/tidewaiter/internal/model"
)

var ctx = context.Background()

func assertEqual[T any](t *testing.T, got, want T) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %#v\nwant %#v", got, want)
	}
}

// What a test builds flows out of, in the kernel's own terms.
const (
	ctaTupleReply = 2
	ctaTupleIP    = 1
	ctaIPv4Src    = 1
	ctaIPv4Dst    = 2
	ctaIPv6Src    = 3
	ctaIPv6Dst    = 4
	ctaProtoSrc   = 2
	ctaTimeout    = 7
	nlaFNested    = 1 << 15

	ipsSeenReply = 1 << 1
	ipsConfirmed = 1 << 3

	// enum tcp_conntrack
	tcpSynSent     = 1
	tcpEstablished = 3
	tcpTimeWait    = 7
)

// attribute is one netlink attribute, padded as the kernel pads it.
func attribute(kind uint16, payload []byte) []byte {
	length := syscall.SizeofRtAttr + len(payload)
	out := make([]byte, (length+3)&^3)
	binary.NativeEndian.PutUint16(out[0:], uint16(length))
	binary.NativeEndian.PutUint16(out[2:], kind)
	copy(out[syscall.SizeofRtAttr:], payload)
	return out
}

// nest is a nested attribute, flagged NLA_F_NESTED as current kernels flag it.
func nest(kind uint16, children ...[]byte) []byte {
	return attribute(kind|nlaFNested, slices.Concat(children...))
}

func be16(value uint16) []byte { return binary.BigEndian.AppendUint16(nil, value) }
func be32(value uint32) []byte { return binary.BigEndian.AppendUint32(nil, value) }

// flow describes one conntrack entry the way a test thinks of it.
type flow struct {
	src, dst     string
	protocol     uint8
	sport, dport uint16
	status       uint32
	tcpState     int // -1 for none, as UDP and ICMP have
}

// tuple is a CTA_TUPLE_ORIG or CTA_TUPLE_REPLY.
func tuple(kind uint16, src, dst netip.Addr, protocol uint8, sport, dport uint16) []byte {
	ip := nest(ctaTupleIP, attribute(ctaIPv4Src, src.AsSlice()), attribute(ctaIPv4Dst, dst.AsSlice()))
	if src.Is6() {
		ip = nest(ctaTupleIP, attribute(ctaIPv6Src, src.AsSlice()), attribute(ctaIPv6Dst, dst.AsSlice()))
	}
	proto := [][]byte{attribute(ctaProtoNum, []byte{protocol})}
	if protocol == syscall.IPPROTO_TCP || protocol == syscall.IPPROTO_UDP {
		proto = append(proto, attribute(ctaProtoSrc, be16(sport)), attribute(ctaProtoDstPort, be16(dport)))
	}
	return nest(kind, ip, nest(ctaTupleProto, proto...))
}

// body is a flow as ctnetlink sends one: nfgenmsg, then the original and reply
// tuples (the reply's ports swapped, so its destination is the client's
// ephemeral port), the status, the timeout and, for TCP, its state.
func (f flow) body() []byte {
	src, dst := netip.MustParseAddr(f.src), netip.MustParseAddr(f.dst)
	family := byte(syscall.AF_INET)
	if src.Is6() {
		family = syscall.AF_INET6
	}
	out := []byte{family, 0, 0, 0}
	out = append(out, tuple(ctaTupleOrig, src, dst, f.protocol, f.sport, f.dport)...)
	out = append(out, tuple(ctaTupleReply, dst, src, f.protocol, f.dport, f.sport)...)
	out = append(out, attribute(ctaStatus, be32(f.status))...)
	out = append(out, attribute(ctaTimeout, be32(120))...)
	if f.tcpState >= 0 {
		out = append(out, nest(ctaProtoinfo, nest(ctaProtoinfoTCP,
			attribute(ctaProtoinfoTCPState, []byte{uint8(f.tcpState)}),
			attribute(2 /* CTA_PROTOINFO_TCP_WSCALE_ORIGINAL, not read */, []byte{7})))...)
	}
	return out
}

// message is one netlink message.
func message(kind uint16, seq uint32, data []byte) []byte {
	out := make([]byte, syscall.NLMSG_HDRLEN, syscall.NLMSG_HDRLEN+len(data)+3)
	binary.NativeEndian.PutUint32(out[0:], uint32(syscall.NLMSG_HDRLEN+len(data)))
	binary.NativeEndian.PutUint16(out[4:], kind)
	binary.NativeEndian.PutUint16(out[6:], syscall.NLM_F_MULTI)
	binary.NativeEndian.PutUint32(out[8:], seq)
	out = append(out, data...)
	for len(out)%4 != 0 {
		out = append(out, 0)
	}
	return out
}

func flowMessage(seq uint32, f flow) []byte {
	return message(nfnlSubsysCTNetlink<<8|ipctnlMsgCTGet, seq, f.body())
}

// done ends a dump, with the errno the kernel puts in it (0, or negated).
func done(seq uint32, errno int32) []byte {
	return message(syscall.NLMSG_DONE, seq, binary.NativeEndian.AppendUint32(nil, uint32(errno)))
}

// failure is an NLMSG_ERROR: the negated errno, then the request it answers.
func failure(seq uint32, errno syscall.Errno) []byte {
	return message(syscall.NLMSG_ERROR, seq, slices.Concat(
		binary.NativeEndian.AppendUint32(nil, uint32(-int32(errno))), request(seq, syscall.AF_INET)))
}

// datagrams is a fake socket: each receive hands over the next datagram.
func datagrams(all ...[]byte) func() ([]byte, error) {
	return func() ([]byte, error) {
		if len(all) == 0 {
			return nil, errors.New("read past the end of the dump")
		}
		next := all[0]
		all = all[1:]
		return next, nil
	}
}

// The flows `conntrack -L` printed in the TypeScript's fixture, plus one
// over IPv6 - the same table, as the kernel sends it.
var (
	established = flow{src: "10.0.0.50", dst: "10.0.0.1", protocol: syscall.IPPROTO_TCP, sport: 51000, dport: 25565,
		status: ipsSeenReply | ipsAssured | ipsConfirmed, tcpState: tcpEstablished}
	synSent = flow{src: "10.0.0.51", dst: "10.0.0.1", protocol: syscall.IPPROTO_TCP, sport: 52000, dport: 25565,
		status: ipsConfirmed, tcpState: tcpSynSent}
	udpAssured = flow{src: "203.0.113.9", dst: "10.0.0.1", protocol: syscall.IPPROTO_UDP, sport: 40000, dport: 19132,
		status: ipsSeenReply | ipsAssured | ipsConfirmed, tcpState: -1}
	udpUnreplied = flow{src: "203.0.113.10", dst: "10.0.0.1", protocol: syscall.IPPROTO_UDP, sport: 40001, dport: 19132,
		status: ipsConfirmed, tcpState: -1}
	http = flow{src: "10.0.0.60", dst: "10.0.0.1", protocol: syscall.IPPROTO_TCP, sport: 44000, dport: 8080,
		status: ipsSeenReply | ipsAssured | ipsConfirmed, tcpState: tcpEstablished}
	timeWait = flow{src: "127.0.0.1", dst: "127.0.0.1", protocol: syscall.IPPROTO_TCP, sport: 52336, dport: 28080,
		status: ipsSeenReply | ipsAssured | ipsConfirmed, tcpState: tcpTimeWait}
	icmp = flow{src: "10.0.0.70", dst: "10.0.0.1", protocol: syscall.IPPROTO_ICMP,
		status: ipsSeenReply | ipsConfirmed, tcpState: -1}
	ipv6 = flow{src: "2001:db8::9", dst: "2001:db8::1", protocol: syscall.IPPROTO_TCP, sport: 60000, dport: 443,
		status: ipsSeenReply | ipsAssured | ipsConfirmed, tcpState: tcpEstablished}
)

// table is the fixture as one dump, in the two datagrams a real dump of it
// might take, ending in NLMSG_DONE.
func table(seq uint32) func() ([]byte, error) {
	return datagrams(
		slices.Concat(flowMessage(seq, established), flowMessage(seq, synSent), flowMessage(seq, udpAssured),
			flowMessage(seq, udpUnreplied)),
		slices.Concat(flowMessage(seq, http), flowMessage(seq, timeWait), flowMessage(seq, icmp),
			flowMessage(seq, ipv6), done(seq, 0)),
	)
}

func TestReadsTCPAndUDPFlowsAndDropsEverythingElse(t *testing.T) {
	entries, err := collect(ctx, table(1), 1)
	if err != nil {
		t.Fatal(err)
	}
	// Eight flows, one of which is ICMP.
	if len(entries) != 7 {
		t.Fatalf("got %d entries, want 7: %v", len(entries), entries)
	}
	for _, entry := range entries {
		if entry.Protocol != model.TCP && entry.Protocol != model.UDP {
			t.Errorf("kept a %q flow", entry.Protocol)
		}
	}
}

func TestTakesTheOriginalDirectionsDestinationPortNotTheReplys(t *testing.T) {
	entries, err := collect(ctx, table(1), 1)
	if err != nil {
		t.Fatal(err)
	}
	var ports []int
	for _, entry := range entries {
		ports = append(ports, entry.DestinationPort)
	}
	slices.Sort(ports)
	// Never a client's ephemeral source port.
	assertEqual(t, ports, []int{443, 8080, 19132, 19132, 25565, 25565, 28080})
}

func TestReadsEveryFieldOfEachFlow(t *testing.T) {
	entries, err := collect(ctx, table(1), 1)
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, entries, []detect.ConntrackEntry{
		{Protocol: model.TCP, DestinationPort: 25565, Assured: true, State: "ESTABLISHED"},
		{Protocol: model.TCP, DestinationPort: 25565, Assured: false, State: "SYN_SENT"},
		{Protocol: model.UDP, DestinationPort: 19132, Assured: true},
		{Protocol: model.UDP, DestinationPort: 19132, Assured: false},
		{Protocol: model.TCP, DestinationPort: 8080, Assured: true, State: "ESTABLISHED"},
		// A closed connection cooling down, which the detector must not count.
		{Protocol: model.TCP, DestinationPort: 28080, Assured: true, State: "TIME_WAIT"},
		{Protocol: model.TCP, DestinationPort: 443, Assured: true, State: "ESTABLISHED"},
	})
}

func TestAnOlderKernelsAnswerOfNewIsReadTheSame(t *testing.T) {
	entries, err := collect(ctx, datagrams(slices.Concat(
		message(nfnlSubsysCTNetlink<<8|ipctnlMsgCTNew, 1, http.body()), done(1, 0))), 1)
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, entries, []detect.ConntrackEntry{{Protocol: model.TCP, DestinationPort: 8080, Assured: true, State: "ESTABLISHED"}})
}

func TestATCPStateTheKernelDoesNotDefineIsLeftEmpty(t *testing.T) {
	odd := http
	odd.tcpState = 200
	entry, ok, err := parseFlow(odd.body())
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	assertEqual(t, entry.State, "")
}

func TestAnEmptyTableIsNoFlowsNotCouldNotTell(t *testing.T) {
	entries, err := collect(ctx, datagrams(done(1, 0)), 1)
	if err != nil {
		t.Fatal(err)
	}
	if entries == nil || len(entries) != 0 {
		t.Fatalf("got %#v, want a non-nil empty table", entries)
	}
}

func TestIgnoresAnswersToOtherRequests(t *testing.T) {
	entries, err := collect(ctx, datagrams(
		slices.Concat(flowMessage(1, http), done(1, 0)),
		slices.Concat(flowMessage(2, established), done(2, 0)),
	), 2)
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, entries, []detect.ConntrackEntry{{Protocol: model.TCP, DestinationPort: 25565, Assured: true, State: "ESTABLISHED"}})
}

func TestAnErrorFromTheKernelIsAnErrorNotAnEmptyTable(t *testing.T) {
	// What an unprivileged dump gets: no CAP_NET_ADMIN.
	entries, err := collect(ctx, datagrams(failure(1, syscall.EPERM)), 1)
	if !errors.Is(err, syscall.EPERM) || entries != nil {
		t.Fatalf("got %v, %v; want EPERM and no table", entries, err)
	}
}

func TestAnAcknowledgementIsNotTheEnd(t *testing.T) {
	entries, err := collect(ctx, datagrams(failure(1, 0), slices.Concat(flowMessage(1, http), done(1, 0))), 1)
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, len(entries), 1)
}

func TestADumpThatFailsPartWayIsAnError(t *testing.T) {
	_, err := collect(ctx, datagrams(slices.Concat(flowMessage(1, http), done(1, -int32(syscall.EINTR)))), 1)
	if !errors.Is(err, syscall.EINTR) {
		t.Fatalf("got %v, want EINTR", err)
	}
}

func TestAReceiveFailureIsAnError(t *testing.T) {
	_, err := collect(ctx, datagrams(flowMessage(1, http)), 1)
	if err == nil {
		t.Fatal("a dump that never finished read as a table")
	}
}

func TestACancelledContextStopsTheDump(t *testing.T) {
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err := collect(cancelled, table(1), 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

func TestMalformedMessagesAreErrorsNotSkipped(t *testing.T) {
	broken := http.body()
	// The first attribute claims more bytes than the message has.
	binary.NativeEndian.PutUint16(broken[nfgenmsgLength:], uint16(len(broken)))
	for name, datagram := range map[string][]byte{
		"an attribute overrunning its message": message(nfnlSubsysCTNetlink<<8|ipctnlMsgCTGet, 1, broken),
		"a message shorter than nfgenmsg":      message(nfnlSubsysCTNetlink<<8|ipctnlMsgCTGet, 1, []byte{2}),
		"a netlink header overrunning":         flowMessage(1, http)[:20],
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := collect(ctx, datagrams(slices.Concat(datagram, done(1, 0))), 1); err == nil {
				t.Fatal("read a malformed dump as a table")
			}
		})
	}
}

func TestAFlowWithNoDestinationPortIsSkipped(t *testing.T) {
	data := []byte{syscall.AF_INET, 0, 0, 0}
	data = append(data, nest(ctaTupleOrig, nest(ctaTupleProto, attribute(ctaProtoNum, []byte{syscall.IPPROTO_TCP})))...)
	_, ok, err := parseFlow(data)
	if err != nil || ok {
		t.Fatalf("got %v, %v; want it skipped", ok, err)
	}
}

func TestTheRequestAsksForADumpOfOneFamily(t *testing.T) {
	got := request(7, syscall.AF_INET6)
	order := binary.NativeEndian
	assertEqual(t, len(got), 20)
	assertEqual(t, order.Uint32(got[0:]), 20)
	assertEqual(t, order.Uint16(got[4:]), 0x0101) // NFNL_SUBSYS_CTNETLINK << 8 | IPCTNL_MSG_CT_GET
	assertEqual(t, order.Uint16(got[6:]), syscall.NLM_F_REQUEST|syscall.NLM_F_DUMP)
	assertEqual(t, order.Uint32(got[8:]), 7)
	assertEqual(t, got[16:], []byte{syscall.AF_INET6, 0, 0, 0})
}

// The real kernel, where this user may read it: in the NixOS VM test as
// root. Anywhere without CAP_NET_ADMIN - a desktop, nix's sandbox - the
// kernel says EPERM, and with no conntrack in the kernel at all something
// else that is not this code's fault.
func TestDumpsTheRealConntrackTable(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback to make a flow on: %v", err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		if conn, err := listener.Accept(); err == nil {
			accepted <- conn
		}
	}()
	client, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	defer func() {
		if conn := <-accepted; conn != nil {
			conn.Close()
		}
	}()

	entries, err := Conntrack().Dump(ctx)
	for _, unavailable := range []error{syscall.EPERM, syscall.EACCES, syscall.EPROTONOSUPPORT, syscall.EAFNOSUPPORT, syscall.EOPNOTSUPP, syscall.ENOENT} {
		if errors.Is(err, unavailable) {
			t.Skipf("no conntrack table to read here: %v", err)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	if entries == nil {
		t.Fatal("a successful dump returned nil, which reads as could-not-tell")
	}
	port := listener.Addr().(*net.TCPAddr).Port
	for _, entry := range entries {
		if entry.DestinationPort < 1 || entry.DestinationPort > 65535 {
			t.Errorf("a flow to port %d", entry.DestinationPort)
		}
		if entry.Protocol == model.TCP && entry.DestinationPort == port {
			// Loopback is tracked only where some rule needs conntrack, so
			// the flow is checked when it is there rather than required.
			assertEqual(t, entry.State, "ESTABLISHED")
			return
		}
	}
	t.Logf("read %d flows; loopback is not tracked here", len(entries))
}
