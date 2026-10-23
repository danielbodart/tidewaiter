package cli

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/danielbodart/tidewaiter/internal/daemon"
	"github.com/danielbodart/tidewaiter/internal/docker"
	"github.com/danielbodart/tidewaiter/internal/nix"
	"github.com/danielbodart/tidewaiter/internal/registry"
	"github.com/danielbodart/tidewaiter/internal/source"
	"github.com/danielbodart/tidewaiter/internal/statestore"
	"github.com/danielbodart/tidewaiter/internal/status"
	"github.com/danielbodart/tidewaiter/internal/systemd"
	"github.com/danielbodart/tidewaiter/internal/version"
)

// Main runs the command line c was given, and is the process's exit code: 2
// for a command line that does not parse, 1 for a failure, 0 otherwise -
// except `status`, whose 0, 1 and 2 are idle, busy and cannot tell.
func Main(ctx context.Context, c *Container) int {
	c.Wire()

	config, err := Parse(c.Args, c.Env)
	if err != nil {
		c.Fail(fmt.Sprintf("Error: %s\n\n%s", err, Usage))
		return 2
	}
	if config.Version {
		fmt.Fprintln(c.Stdout, version.Version)
		return 0
	}
	if config.Help {
		fmt.Fprintln(c.Stdout, Usage)
		return 0
	}

	// Before the banner: its output is for scripts as much as for people.
	if config.Command == "status" {
		code, err := status.Command(cmp.Or(config.StatusFile, status.DefaultFile), c.Clock.Now(), c.Stdout)
		if err != nil {
			// Not 1, which says something is busy: a status that cannot be
			// read says nothing either way.
			c.Fail("Error: " + err.Error())
			return status.Unknown
		}
		return code
	}

	c.Note("Tidewaiter " + version.Version)
	code, err := run(ctx, c, config)
	if err != nil {
		c.Fail("Error: " + err.Error())
		return 1
	}
	return code
}

func run(ctx context.Context, c *Container, config Config) (int, error) {
	// Opened only when a source or command needs it: the container has no bus.
	var bus systemd.Bus
	systemdBus := func() (systemd.Bus, error) {
		if bus != nil {
			return bus, nil
		}
		opened, err := c.Bus(c.Env)
		if err != nil {
			return nil, err
		}
		bus = opened
		return bus, nil
	}
	defer func() {
		if closer, ok := bus.(io.Closer); ok {
			closer.Close()
		}
	}()
	profiles := nix.NixProfiles{Run: c.Nix}
	closure := nix.NixClosureBinder{RuntimeDir: c.RuntimeDir, Run: c.Nix, PID: c.PID}
	units := systemd.Dir{Path: config.UnitsDir}

	if config.Command == "prepare" {
		reloader, err := systemdBus()
		if err != nil {
			return 0, err
		}
		prepared, err := systemd.Prepare(ctx, systemd.PrepareDeps{
			Units: units, Profiles: profiles, Closure: closure, Systemd: reloader, Log: c.Note,
		})
		if err != nil {
			return 0, err
		}
		c.Note(fmt.Sprintf("Prepared %s: %d profile(s) created, %d closure drop-in(s) written, %d failed",
			config.UnitsDir, len(prepared.Seeded), len(prepared.Bound), len(prepared.Failed)))
		if len(prepared.Failed) > 0 {
			return 1, nil
		}
		return 0, nil
	}

	var sources []source.Source
	for _, name := range config.Sources {
		switch name {
		case source.Docker:
			// One credential source, shared by the registry client (for
			// digest lookups) and the source (for an authenticated pull), so
			// a private image both resolves and pulls with the same creds.
			auth := registry.DockerConfigAuth(c.DockerConfig)
			options := docker.DefaultOptions
			options.Label = config.Label
			options.DryRun = config.DryRun
			sources = append(sources, docker.NewSource(docker.Deps{
				Docker:   docker.NewClient(c.DockerTransport(config.Socket)),
				Registry: registry.New(c.RegistryTransport, auth, c.Clock),
				Auth:     auth,
				Netns:    c.Netns,
				TCP:      c.TCP,
				Options:  options,
				Log:      c.Note,
				Clock:    c.Clock,
			}))

		case source.Systemd:
			bus, err := systemdBus()
			if err != nil {
				return 0, err
			}
			sources = append(sources, systemd.New(systemd.Deps{
				Units:     units,
				Bus:       bus,
				Profiles:  profiles,
				Closure:   closure,
				Sockets:   c.Sockets,
				Conntrack: c.Conntrack,
				Store:     statestore.File{Path: filepath.Join(config.StateDir, statestore.FileName)},
				Netns:     c.Netns,
				TCP:       c.TCP,
				Log:       c.Note,
				Clock:     c.Clock,
			}, systemd.Options{DryRun: config.DryRun, HealthPoll: docker.DefaultOptions.HealthPoll}))
		}
	}

	if config.Command == "list" {
		var lines []string
		for _, s := range sources {
			listed, err := s.List(ctx)
			if err != nil {
				return 0, err
			}
			lines = append(lines, listed...)
		}
		switch {
		case len(lines) > 0:
			c.Note(strings.Join(lines, "\n"))
		case len(config.Sources) > 1:
			c.Note("Nothing is opted in.")
		case config.Sources[0] == source.Docker:
			c.Note("No opted-in containers.")
		default:
			c.Note("No managed units.")
		}
		return 0, nil
	}

	// Only `run` writes a status, or reboots: a `check` beside a running
	// daemon must not overwrite what the daemon says.
	running := config.Command == "run"
	deps := daemon.Deps{
		Sources:   sources,
		Conntrack: c.Conntrack,
		Options:   config.Daemon,
		Log:       c.Note,
		Status:    status.None,
		Clock:     c.Clock,
	}
	if running && config.StatusFile != "" {
		deps.Status = status.File{Path: config.StatusFile, PID: c.PID}
	}
	if running && config.AutoRebootWindow != "" {
		rebooter, err := c.Rebooter(c.Env)
		if err != nil {
			return 0, err
		}
		if closer, ok := rebooter.(io.Closer); ok {
			defer closer.Close()
		}
		deps.AutoReboot = &daemon.AutoReboot{
			Window:   config.Window,
			Label:    config.AutoRebootWindow,
			Check:    c.BootCheck(config.BootedSystem, config.CurrentSystem),
			Rebooter: rebooter,
		}
	}
	tidewaiter := daemon.New(deps)
	c.Note("Sources: " + joinNames(config.Sources))
	if deps.AutoReboot != nil {
		c.Note(fmt.Sprintf("Rebooting automatically when needed, between %s, once nothing is in use", config.AutoRebootWindow))
	}
	if config.DryRun {
		c.Note("Dry run - nothing will be changed")
	}

	if config.Command == "check" {
		if _, err := tidewaiter.Once(ctx); err != nil {
			return 0, err
		}
		return 0, nil
	}

	// run: Parse has refused anything else.
	ctx, stop := untilInterrupted(ctx, c)
	defer stop()
	tidewaiter.Run(ctx)
	return 0, nil
}

// untilInterrupted is ctx, cancelled by the first SIGINT or SIGTERM.
//
// Run finishes the pass in flight before it returns, so stopping never leaves
// a container stopped and not yet replaced, or a unit's profile moved and the
// unit not yet restarted. Every signal stays caught until then - a second
// Ctrl-C is only said, not obeyed - since the unit's TimeoutStopSec, and
// Docker's stop timeout, are what bound a shutdown that is taking too long.
func untilInterrupted(ctx context.Context, c *Container) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	signals, release := c.Signals()
	quit, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		for {
			select {
			case received := <-signals:
				c.Note(fmt.Sprintf("\nReceived %s, shutting down...", signalName(received)))
				cancel()
			case <-quit:
				return
			}
		}
	}()
	return ctx, func() {
		close(quit)
		<-finished
		release()
		cancel()
	}
}

func signalName(received os.Signal) string {
	switch received {
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGTERM:
		return "SIGTERM"
	}
	return received.String()
}
