package systemd

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/danielbodart/gokit/dbusx"
	"github.com/danielbodart/tidewaiter/internal/reboot"
	"github.com/godbus/dbus/v5"
)

const (
	serviceIface = "org.freedesktop.systemd1.Service"

	login1        = "org.freedesktop.login1"
	login1Path    = dbus.ObjectPath("/org/freedesktop/login1")
	login1Manager = "org.freedesktop.login1.Manager"
)

// jobRemoved is every JobRemoved systemd sends, which Subscribe turns on for
// the connection that asked.
const jobRemoved = "type='signal',sender='" + dbusx.Systemd + "',interface='" + dbusx.SystemdManager + "',member='JobRemoved',path='" + string(dbusx.SystemdPath) + "'"

// SystemBus is systemd and logind over the system bus at
// DBUS_SYSTEM_BUS_ADDRESS, the variable libdbus and sd-bus read, or the
// standard socket - unix: addresses only, as dbusx.SystemBusAddress takes
// them.
//
// Nothing is connected until the first call, so a command that never needs
// systemd - and the container, which has no bus - never opens one.
func SystemBus(env map[string]string) (*DBus, error) {
	address, err := dbusx.SystemBusAddress(env)
	if err != nil {
		return nil, err
	}
	return &DBus{address: address}, nil
}

// DBus is systemd's Manager, and logind's, over D-Bus.
//
// The Manager's calls share one connection, opened when first needed and
// again after it drops, that has subscribed to JobRemoved: a restart is
// RestartUnit followed by waiting for the job it returned to be removed, and
// the subscription is in place before the first job is ever queued, so its end
// cannot be missed. Restarting another unit needs root (or polkit's say-so),
// which the daemon has.
type DBus struct {
	address string

	mu     sync.Mutex
	shared *connection
}

var (
	_ Bus             = (*DBus)(nil)
	_ reboot.Rebooter = (*DBus)(nil)
)

// connection is one connection to the bus, and the restarts waiting on it.
type connection struct {
	conn *dbus.Conn
	// Closed once the connection has gone, after which no job's end can be
	// heard on it.
	gone chan struct{}

	mu sync.Mutex
	// Restarts waiting on their job, by job path.
	waiting map[dbus.ObjectPath]chan JobResult
	// Jobs that ended while a restart was in flight but before its job path
	// was known - the reply and the signal are separate messages, and nothing
	// promises the reply comes first. Emptied whenever no restart is in flight.
	ended    map[dbus.ObjectPath]JobResult
	inFlight int
}

func (b *DBus) connect(ctx context.Context) (*dbus.Conn, error) {
	// Signals in order, as systemd sent them.
	return dbusx.Connect(b.address, dbus.WithContext(ctx))
}

func (b *DBus) connection(ctx context.Context) (*connection, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.shared != nil {
		select {
		case <-b.shared.gone:
		default:
			return b.shared, nil
		}
	}
	conn, err := b.connect(context.WithoutCancel(ctx))
	if err != nil {
		return nil, err
	}
	c := &connection{
		conn:    conn,
		gone:    make(chan struct{}),
		waiting: map[dbus.ObjectPath]chan JobResult{},
		ended:   map[dbus.ObjectPath]JobResult{},
	}
	signals := make(chan *dbus.Signal, 64)
	conn.Signal(signals)
	go c.listen(signals)

	// Without the subscription systemd sends no JobRemoved at all. It lasts
	// as long as this connection does.
	if err := dbusx.Subscribe(ctx, conn, jobRemoved); err != nil {
		conn.Close()
		return nil, err
	}
	b.shared = c
	return c, nil
}

// listen hands each job's end to the restart waiting for it, until the
// connection goes.
func (c *connection) listen(signals <-chan *dbus.Signal) {
	for signal := range signals {
		removed, ok := toJobRemoved(signal)
		if !ok {
			continue
		}
		c.mu.Lock()
		if waiter, found := c.waiting[removed.job]; found {
			delete(c.waiting, removed.job)
			waiter <- removed.result
		} else if c.inFlight > 0 {
			c.ended[removed.job] = removed.result
		}
		c.mu.Unlock()
	}
	close(c.gone)
}

// Close closes the shared connection, if there is one.
func (b *DBus) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.shared == nil {
		return nil
	}
	err := b.shared.conn.Close()
	b.shared = nil
	return err
}

func (b *DBus) Unit(ctx context.Context, name string) (UnitState, error) {
	c, err := b.connection(ctx)
	if err != nil {
		return UnitState{}, err
	}
	// LoadUnit, not GetUnit: a unit nothing has started yet is not loaded,
	// and GetUnit would call it missing. A name with no unit file comes back
	// loaded as "not-found", which is the answer.
	var path dbus.ObjectPath
	if err := c.conn.Object(dbusx.Systemd, dbusx.SystemdPath).CallWithContext(ctx, dbusx.SystemdManager+".LoadUnit", 0, name).Store(&path); err != nil {
		return UnitState{}, fmt.Errorf("loading %s: %w", name, dbusx.Describe(err))
	}
	unit, err := getAll(ctx, c.conn, path, dbusx.SystemdUnit)
	if err != nil {
		return UnitState{}, fmt.Errorf("reading %s: %w", name, err)
	}
	var service map[string]dbus.Variant
	if strings.HasSuffix(name, ".service") {
		if service, err = getAll(ctx, c.conn, path, serviceIface); err != nil {
			return UnitState{}, fmt.Errorf("reading %s: %w", name, err)
		}
	}
	return toUnitState(name, unit, service), nil
}

func getAll(ctx context.Context, conn *dbus.Conn, path dbus.ObjectPath, iface string) (map[string]dbus.Variant, error) {
	var all map[string]dbus.Variant
	if err := conn.Object(dbusx.Systemd, path).CallWithContext(ctx, dbusx.Properties+".GetAll", 0, iface).Store(&all); err != nil {
		return nil, dbusx.Describe(err)
	}
	return all, nil
}

func (b *DBus) Restart(ctx context.Context, name string) (JobResult, error) {
	c, err := b.connection(ctx)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	c.inFlight++
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.inFlight--
		if c.inFlight == 0 {
			clear(c.ended)
		}
	}()

	var job dbus.ObjectPath
	if err := c.conn.Object(dbusx.Systemd, dbusx.SystemdPath).CallWithContext(ctx, dbusx.SystemdManager+".RestartUnit", 0, name, "replace").Store(&job); err != nil {
		return "", fmt.Errorf("restarting %s: %w", name, dbusx.Describe(err))
	}
	c.mu.Lock()
	if result, already := c.ended[job]; already {
		c.mu.Unlock()
		return result, nil
	}
	ended := make(chan JobResult, 1)
	c.waiting[job] = ended
	c.mu.Unlock()

	select {
	case result := <-ended:
		return result, nil
	case <-c.gone:
		// A job's end can no longer be heard on this connection; the caller
		// decides what that means (see the source's undo).
		select {
		case result := <-ended:
			return result, nil
		default:
		}
		return "", errors.New("lost the bus while waiting for the restart job (D-Bus connection closed)")
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.waiting, job)
		c.mu.Unlock()
		return "", ctx.Err()
	}
}

func (b *DBus) DaemonReload(ctx context.Context) error {
	c, err := b.connection(ctx)
	if err != nil {
		return err
	}
	// Answered once the reload is done, so the drop-in just written is in
	// force by the time this returns.
	if err := c.conn.Object(dbusx.Systemd, dbusx.SystemdPath).CallWithContext(ctx, dbusx.SystemdManager+".Reload", 0).Err; err != nil {
		return fmt.Errorf("reloading systemd: %w", dbusx.Describe(err))
	}
	return nil
}

// Reboot is logind's Manager.Reboot, as `systemctl reboot` asks for it: the
// same shutdown everything else takes, and the same block inhibitors -
// someone else's lock still refuses it, and the next pass in the window tries
// again. Not interactive: there is nobody to ask for a password.
//
// On a connection of its own, closed after: a reboot needs none of the
// Manager's subscription, and is asked for at most once a pass.
func (b *DBus) Reboot(ctx context.Context) error {
	conn, err := b.connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.Object(login1, login1Path).CallWithContext(ctx, login1Manager+".Reboot", 0, false).Err; err != nil {
		return fmt.Errorf("asking logind to reboot: %w", dbusx.Describe(err))
	}
	return nil
}

// removal is a JobRemoved signal's job path, unit and result.
type removal struct {
	job    dbus.ObjectPath
	unit   string
	result JobResult
}

// toJobRemoved reads a JobRemoved signal, and nothing else.
func toJobRemoved(signal *dbus.Signal) (removal, bool) {
	if signal.Name != dbusx.SystemdManager+".JobRemoved" || len(signal.Body) < 4 {
		return removal{}, false
	}
	job, ok := dbusx.Arg[dbus.ObjectPath](signal.Body, 1)
	if !ok {
		return removal{}, false
	}
	unit, _ := dbusx.Arg[string](signal.Body, 2)
	said, _ := dbusx.Arg[string](signal.Body, 3)
	// A result this code does not know is not a success.
	result := JobFailed
	for _, known := range jobResults {
		if string(known) == said {
			result = known
		}
	}
	return removal{job: job, unit: unit, result: result}, true
}

// toUnitState is the properties Tidewaiter reads, from GetAll on the Unit
// and Service interfaces. Missing, or of another type, reads as nothing, as
// for a unit that is not running. InvocationID arrives as 16 bytes and is
// kept as hex, as `systemctl show` prints it.
func toUnitState(name string, unit, service map[string]dbus.Variant) UnitState {
	text := func(properties map[string]dbus.Variant, key string) string {
		value, _ := properties[key].Value().(string)
		return value
	}
	count := func(key string) int {
		switch value := service[key].Value().(type) {
		case uint32:
			return int(value)
		case int32:
			return int(value)
		case uint64:
			return int(value)
		case int64:
			return int(value)
		}
		return 0
	}
	invocation, _ := unit["InvocationID"].Value().([]byte)
	return UnitState{
		Name:         name,
		LoadState:    text(unit, "LoadState"),
		ActiveState:  text(unit, "ActiveState"),
		SubState:     text(unit, "SubState"),
		Result:       text(service, "Result"),
		NRestarts:    count("NRestarts"),
		MainPID:      count("MainPID"),
		ControlGroup: text(service, "ControlGroup"),
		InvocationID: hex.EncodeToString(invocation),
	}
}
