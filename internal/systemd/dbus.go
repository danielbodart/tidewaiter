package systemd

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/danielbodart/tidewaiter/internal/reboot"
	"github.com/godbus/dbus/v5"
)

const (
	systemd1        = "org.freedesktop.systemd1"
	managerPath     = dbus.ObjectPath("/org/freedesktop/systemd1")
	manager         = "org.freedesktop.systemd1.Manager"
	unitIface       = "org.freedesktop.systemd1.Unit"
	serviceIface    = "org.freedesktop.systemd1.Service"
	propertiesIface = "org.freedesktop.DBus.Properties"
	unitPaths       = "/org/freedesktop/systemd1/unit"

	login1        = "org.freedesktop.login1"
	login1Path    = dbus.ObjectPath("/org/freedesktop/login1")
	login1Manager = "org.freedesktop.login1.Manager"

	// DefaultSystemBus is where the system bus is when nothing says otherwise.
	DefaultSystemBus = "unix:path=/run/dbus/system_bus_socket"
)

// jobRemoved is every JobRemoved systemd sends, which Subscribe turns on for
// the connection that asked.
const jobRemoved = "type='signal',sender='" + systemd1 + "',interface='" + manager + "',member='JobRemoved',path='" + string(managerPath) + "'"

// SystemBus is systemd and logind over the system bus at
// DBUS_SYSTEM_BUS_ADDRESS, the variable libdbus and sd-bus read, or the
// standard socket.
//
// Only unix: addresses are taken, which is all a system bus has ever been:
// anything else is far more likely a mistake than a bus. Nothing is connected
// until the first call, so a command that never needs systemd - and the
// container, which has no bus - never opens one.
func SystemBus(env map[string]string) (*DBus, error) {
	address := env["DBUS_SYSTEM_BUS_ADDRESS"]
	if address == "" {
		address = DefaultSystemBus
	}
	var usable []string
	for candidate := range strings.SplitSeq(address, ";") {
		if strings.HasPrefix(candidate, "unix:") {
			usable = append(usable, candidate)
		}
	}
	if len(usable) == 0 {
		return nil, fmt.Errorf("cannot use DBUS_SYSTEM_BUS_ADDRESS '%s': only unix: addresses are supported", address)
	}
	return &DBus{address: strings.Join(usable, ";")}, nil
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
	// In order, as systemd sent them; the default handler hands a busy
	// reader's signals to goroutines.
	conn, err := dbus.Connect(b.address, dbus.WithContext(ctx), dbus.WithSignalHandler(dbus.NewSequentialSignalHandler()))
	if err != nil {
		return nil, fmt.Errorf("D-Bus at %s: %w", b.address, err)
	}
	return conn, nil
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

	if err := conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.AddMatch", 0, jobRemoved).Err; err != nil {
		conn.Close()
		return nil, fmt.Errorf("asking for systemd's job signals: %w", describe(err))
	}
	// Without this systemd sends no JobRemoved at all. It lasts as long as
	// this connection does.
	if err := conn.Object(systemd1, managerPath).CallWithContext(ctx, manager+".Subscribe", 0).Err; err != nil {
		conn.Close()
		return nil, fmt.Errorf("subscribing to systemd: %w", describe(err))
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
	if err := c.conn.Object(systemd1, managerPath).CallWithContext(ctx, manager+".LoadUnit", 0, name).Store(&path); err != nil {
		return UnitState{}, fmt.Errorf("loading %s: %w", name, describe(err))
	}
	unit, err := getAll(ctx, c.conn, path, unitIface)
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
	if err := conn.Object(systemd1, path).CallWithContext(ctx, propertiesIface+".GetAll", 0, iface).Store(&all); err != nil {
		return nil, describe(err)
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
	if err := c.conn.Object(systemd1, managerPath).CallWithContext(ctx, manager+".RestartUnit", 0, name, "replace").Store(&job); err != nil {
		return "", fmt.Errorf("restarting %s: %w", name, describe(err))
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
	if err := c.conn.Object(systemd1, managerPath).CallWithContext(ctx, manager+".Reload", 0).Err; err != nil {
		return fmt.Errorf("reloading systemd: %w", describe(err))
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
		return fmt.Errorf("asking logind to reboot: %w", describe(err))
	}
	return nil
}

// describe adds the error's name to a D-Bus error, whose own text is only its
// message - "Operation denied due to active block inhibitor" says less
// without org.freedesktop.login1.OperationInProgress.
func describe(err error) error {
	var named dbus.Error
	if errors.As(err, &named) {
		return fmt.Errorf("%s: %w", named.Name, err)
	}
	var pointer *dbus.Error
	if errors.As(err, &pointer) {
		return fmt.Errorf("%s: %w", pointer.Name, err)
	}
	return err
}

// removal is a JobRemoved signal's job path, unit and result.
type removal struct {
	job    dbus.ObjectPath
	unit   string
	result JobResult
}

// toJobRemoved reads a JobRemoved signal, and nothing else.
func toJobRemoved(signal *dbus.Signal) (removal, bool) {
	if signal.Name != manager+".JobRemoved" || len(signal.Body) < 4 {
		return removal{}, false
	}
	job, ok := signal.Body[1].(dbus.ObjectPath)
	if !ok {
		return removal{}, false
	}
	unit, _ := signal.Body[2].(string)
	said, _ := signal.Body[3].(string)
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

// unitPath is a unit's object path, escaped as systemd escapes it: every
// byte that is not a letter or digit, and a leading digit, becomes `_` and
// two hex digits. Only for tests: calls go through LoadUnit, which returns
// the path itself.
func unitPath(name string) dbus.ObjectPath {
	var escaped strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || i > 0 && c >= '0' && c <= '9' {
			escaped.WriteByte(c)
		} else {
			fmt.Fprintf(&escaped, "_%02x", c)
		}
	}
	return dbus.ObjectPath(unitPaths + "/" + escaped.String())
}
