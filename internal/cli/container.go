package cli

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/danielbodart/gokit/clock"
	"github.com/danielbodart/gokit/httpx"
	"github.com/danielbodart/gokit/sockdiag"
	"github.com/danielbodart/tidewaiter/internal/activity"
	"github.com/danielbodart/tidewaiter/internal/detect"
	"github.com/danielbodart/tidewaiter/internal/health"
	"github.com/danielbodart/tidewaiter/internal/nix"
	"github.com/danielbodart/tidewaiter/internal/reboot"
	"github.com/danielbodart/tidewaiter/internal/systemd"
)

// Container is the one object graph. Production wires it once; each test
// wires a fresh one for an isolated graph. Leaves left nil get production
// defaults when Wire runs, so a test sets them to doubles *before* wiring.
// Everything else is built from the leaves by Main, never set.
type Container struct {
	// The command line, without the program name, and the environment.
	Args []string
	Env  map[string]string
	// What the daemon says goes to Stdout, as it always has, so the journal
	// and `docker logs` read as before; an error that ends a command goes to
	// Stderr.
	Stdout, Stderr io.Writer
	// Time, for the intervals, the gates, the reboot window and how old a
	// status is.
	Clock clock.Clock
	// Names this process's temporary files - the status file's, a closure
	// drop-in's - so two writers never share one.
	PID int

	// How the Docker Engine API is reached at a socket path.
	DockerTransport func(socket string) http.RoundTripper
	// How image registries are reached, for digests; the client bounds each
	// lookup itself.
	RegistryTransport http.RoundTripper
	// ~/.docker/config.json, for registry credentials; an error is anonymous.
	DockerConfig func() ([]byte, error)

	// The system bus, from the environment: systemd, for the systemd source
	// and prepare.
	Bus func(env map[string]string) (systemd.Bus, error)
	// logind, for an automatic reboot.
	Rebooter func(env map[string]string) (reboot.Rebooter, error)
	// Whether the installed NixOS needs a reboot, comparing the two systems.
	BootCheck func(booted, current string) reboot.BootCheck
	// nix-env and nix-store.
	Nix nix.Runner
	// Where closure drop-ins go: systemd's runtime unit directory.
	RuntimeDir string

	// The host's conntrack table, once per pass.
	Conntrack detect.ConntrackDumper
	// A process's /proc/<pid>/net/* tables.
	Netns detect.Netns
	// Listening sockets and their cgroups, for a unit's ports.
	Sockets sockdiag.Lister
	// Connects, for port-connect.
	TCP health.Prober

	// SIGINT and SIGTERM as they arrive, and how to stop listening for them.
	Signals func() (<-chan os.Signal, func())

	speaking sync.Mutex
}

// Wire fills in the leaves left unset.
func (c *Container) Wire() {
	if c.Args == nil {
		c.Args = os.Args[1:]
	}
	if c.Env == nil {
		c.Env = environ()
	}
	if c.Stdout == nil {
		c.Stdout = os.Stdout
	}
	if c.Stderr == nil {
		c.Stderr = os.Stderr
	}
	if c.Clock == nil {
		c.Clock = clock.System{}
	}
	if c.PID == 0 {
		c.PID = os.Getpid()
	}
	if c.DockerTransport == nil {
		c.DockerTransport = httpx.UnixSocket
	}
	if c.RegistryTransport == nil {
		c.RegistryTransport = http.DefaultTransport.(*http.Transport).Clone()
	}
	if c.DockerConfig == nil {
		env := c.Env
		c.DockerConfig = func() ([]byte, error) {
			home := env["HOME"]
			if home == "" {
				return nil, errors.New("no HOME, so no ~/.docker/config.json")
			}
			return os.ReadFile(filepath.Join(home, ".docker", "config.json"))
		}
	}
	if c.Bus == nil {
		c.Bus = func(env map[string]string) (systemd.Bus, error) { return systemd.SystemBus(env) }
	}
	if c.Rebooter == nil {
		c.Rebooter = func(env map[string]string) (reboot.Rebooter, error) { return systemd.SystemBus(env) }
	}
	if c.BootCheck == nil {
		c.BootCheck = func(booted, current string) reboot.BootCheck {
			return reboot.NixOS{Booted: booted, Current: current}
		}
	}
	if c.Nix == nil {
		c.Nix = nix.Exec{}
	}
	if c.RuntimeDir == "" {
		c.RuntimeDir = nix.DefaultRuntimeDir
	}
	if c.Conntrack == nil {
		c.Conntrack = activity.Conntrack()
	}
	if c.Netns == nil {
		// The host's /proc, bind-mounted read-only at /host/proc in the
		// container, is what lets us read another container's
		// /proc/<pid>/net/* (host networking shares the net ns, not the pid
		// ns). Falls back to /proc when running native.
		c.Netns = activity.ProcNetns{Root: activity.ProcRoot()}
	}
	if c.Sockets == nil {
		c.Sockets = sockdiag.Kernel()
	}
	if c.TCP == nil {
		c.TCP = health.Dialer{}
	}
	if c.Signals == nil {
		c.Signals = interrupts
	}
}

// Note is a line of what Tidewaiter is doing, on stdout.
//
// The daemon's passes and ticks, background pulls and the signal handler all
// speak through here from their own goroutines, so one line is written at a
// time.
func (c *Container) Note(line string) {
	c.speaking.Lock()
	defer c.speaking.Unlock()
	fmt.Fprintln(c.Stdout, line)
}

// Fail is an error that ends a command, on stderr.
func (c *Container) Fail(line string) {
	c.speaking.Lock()
	defer c.speaking.Unlock()
	fmt.Fprintln(c.Stderr, line)
}

func environ() map[string]string {
	env := map[string]string{}
	for _, entry := range os.Environ() {
		if name, value, ok := strings.Cut(entry, "="); ok {
			env[name] = value
		}
	}
	return env
}

// interrupts is SIGINT and SIGTERM, caught rather than fatal.
func interrupts() (<-chan os.Signal, func()) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	return signals, func() { signal.Stop(signals) }
}
