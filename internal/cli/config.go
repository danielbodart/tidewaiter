// Package cli is Tidewaiter's command line: what the arguments mean, and the
// object graph a command runs on.
package cli

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danielbodart/tidewaiter/internal/daemon"
	"github.com/danielbodart/tidewaiter/internal/docker"
	"github.com/danielbodart/tidewaiter/internal/reboot"
	"github.com/danielbodart/tidewaiter/internal/source"
	"github.com/danielbodart/tidewaiter/internal/status"
)

const (
	DefaultDockerSocket = "/var/run/docker.sock"
	DefaultUnitsDir     = "/etc/tidewaiter/units"
	DefaultStateDir     = "/var/lib/tidewaiter"
)

// Usage is what --help prints, and what follows a command line that does not
// parse.
var Usage = `tidewaiter - update Docker containers and systemd units only when they are idle

Usage: tidewaiter [options] [command]

Commands:
  run       Reconcile continuously on an interval (default)
  check     Reconcile once and exit
  list      Show managed containers and units, and whether an update is waiting
  status    Show what the running daemon sees in use or swapping; exit 0 when
            nothing is, 1 when something is, 2 when no daemon is writing one
  prepare   Create missing unit profiles and missing closure drop-ins, then exit

Options:
  -s, --source NAME       docker or systemd; repeat or comma-separate for both
                          (default docker)
  -d, --interval SECONDS  Seconds between reconcile passes (default ` + seconds(daemon.Defaults.Interval) + `)
  -n, --dry-run           Report what would change without changing it
      --status-file PATH  Where run keeps what is in use and swapping, and where
                          status reads it (status's default ` + status.DefaultFile + `;
                          run writes none unless given one)
      --activity-interval SECONDS
                          Seconds between activity checks between passes, with a
                          status file or an automatic reboot (default ` + seconds(daemon.Defaults.ActivityInterval) + `)
      --auto-reboot-window HH:MM-HH:MM
                          Reboot when the installed NixOS needs it (its kernel,
                          initrd or modules changed since boot), only inside this
                          daily window of local time, and only once everything is
                          idle and nothing is being swapped
      --booted-system P   The system that booted (default ` + reboot.Defaults.Booted + `)
      --current-system P  The system installed (default ` + reboot.Defaults.Current + `)
      --version           Show the version and exit
  -h, --help              Show this message

Docker source:
  -l, --label LABEL       Opt-in label to read (default ` + docker.DefaultOptions.Label + `)
      --docker-socket P   Docker socket (default ` + DefaultDockerSocket + `)

systemd source:
      --units-dir DIR     Where managed units are declared (default ` + DefaultUnitsDir + `)
      --state-dir DIR     Where pins are kept (default ` + DefaultStateDir + `)

Environment:
  TIDEWAITER_SOURCES      Same as --source
  TIDEWAITER_INTERVAL     Same as --interval
  TIDEWAITER_STATUS_FILE  Same as --status-file
  TIDEWAITER_AUTO_REBOOT_WINDOW
                          Same as --auto-reboot-window
  DOCKER_SOCKET           Same as --docker-socket
  TIDEWAITER_UNITS_DIR    Same as --units-dir
  STATE_DIRECTORY         Same as --state-dir, as systemd's StateDirectory= sets it

Opt a container in with 'tidewaiter.autoupdate=registry' (or =local). Tidewaiter
compares the image the container runs against what its tag should resolve to -
the registry's digest, or the locally-stored image for 'local' - and when they
differ it waits until the container is idle (no live network flows), pulls (for
registry), recreates, health-checks, and rolls back if the new image is
unhealthy. A container with no autoupdate label is never touched.

Opt a systemd unit in with a JSON file in the units directory naming the unit,
the Nix profile its ExecStart runs from, and the store path that profile should
point at. When they differ Tidewaiter waits until the unit is idle, sets the
profile, restarts the unit, health-checks it, and on failure switches the
profile back and restarts it again. The NixOS module writes these files.`

func seconds(d time.Duration) string { return strconv.Itoa(int(d / time.Second)) }

// Commands is every command there is.
var Commands = []string{"run", "check", "list", "status", "prepare"}

// Config is what a command line and the environment say to do.
type Config struct {
	Command string
	// In the order given, each once.
	Sources []source.Name
	Daemon  daemon.Options
	// The Docker source's opt-in label.
	Label  string
	DryRun bool
	Socket string
	// The systemd source's declarations and pins.
	UnitsDir, StateDir string
	// "" unless given: run writes no status, and status reads the default.
	StatusFile string
	// "" unless given; Window is it parsed.
	AutoRebootWindow string
	Window           reboot.Window
	BootedSystem     string
	CurrentSystem    string
	Help, Version    bool
}

// legacyEntrypoint is a leading path to the binary, as an overridden
// entrypoint might pass, which is skipped rather than mistaken for a command.
var legacyEntrypoint = regexp.MustCompile(`/tidewaiter$`)

// Parse reads args (without the program name) over env.
//
// Everything is checked here, so a typo stops the daemon at start rather than
// at 3am: numbers are whole seconds, sources and the command are ones there
// are, and the reboot window is a window.
func Parse(args []string, env map[string]string) (Config, error) {
	config := Config{
		Command:       "run",
		Daemon:        daemon.Defaults,
		Label:         docker.DefaultOptions.Label,
		Socket:        cmp.Or(env["DOCKER_SOCKET"], DefaultDockerSocket),
		UnitsDir:      cmp.Or(env["TIDEWAITER_UNITS_DIR"], DefaultUnitsDir),
		StatusFile:    env["TIDEWAITER_STATUS_FILE"],
		BootedSystem:  reboot.Defaults.Booted,
		CurrentSystem: reboot.Defaults.Current,
	}
	// systemd joins several StateDirectory= entries with colons; the first is
	// ours.
	first, _, _ := strings.Cut(env["STATE_DIRECTORY"], ":")
	config.StateDir = cmp.Or(first, DefaultStateDir)
	config.AutoRebootWindow = env["TIDEWAITER_AUTO_REBOOT_WINDOW"]
	if text := env["TIDEWAITER_INTERVAL"]; text != "" {
		interval, err := positiveSeconds(text, "TIDEWAITER_INTERVAL")
		if err != nil {
			return Config{}, err
		}
		config.Daemon.Interval = interval
	}

	var sources []source.Name
	for i := 0; i < len(args); i++ {
		argument := args[i]
		value := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", argument)
			}
			i++
			return args[i], nil
		}
		var text string
		var err error
		switch argument {
		case "-s", "--source", "-d", "--interval", "--activity-interval", "-l", "--label",
			"--docker-socket", "--units-dir", "--state-dir", "--status-file",
			"--auto-reboot-window", "--booted-system", "--current-system":
			if text, err = value(); err != nil {
				return Config{}, err
			}
		}
		switch argument {
		case "-s", "--source":
			names, err := sourceNames(text, argument)
			if err != nil {
				return Config{}, err
			}
			sources = append(sources, names...)
		case "-d", "--interval":
			if config.Daemon.Interval, err = positiveSeconds(text, argument); err != nil {
				return Config{}, err
			}
		case "--activity-interval":
			if config.Daemon.ActivityInterval, err = positiveSeconds(text, argument); err != nil {
				return Config{}, err
			}
		case "-l", "--label":
			config.Label = text
		case "--docker-socket":
			config.Socket = text
		case "--units-dir":
			config.UnitsDir = text
		case "--state-dir":
			config.StateDir = text
		case "--status-file":
			config.StatusFile = text
		case "--auto-reboot-window":
			config.AutoRebootWindow = text
		case "--booted-system":
			config.BootedSystem = text
		case "--current-system":
			config.CurrentSystem = text
		case "-n", "--dry-run":
			config.DryRun = true
		case "--version":
			config.Version = true
		case "-h", "--help":
			config.Help = true
		default:
			if strings.HasPrefix(argument, "-") {
				return Config{}, fmt.Errorf("unknown option '%s'", argument)
			}
			if legacyEntrypoint.MatchString(argument) {
				continue
			}
			// The last word wins, as it always has: a compose file whose
			// command repeats the binary's name before `run` still runs.
			config.Command = argument
		}
	}

	// Docker by default, so the image and every existing compose file behave
	// exactly as they did before there was a choice.
	if sources == nil {
		if text := env["TIDEWAITER_SOURCES"]; text != "" {
			names, err := sourceNames(text, "TIDEWAITER_SOURCES")
			if err != nil {
				return Config{}, err
			}
			sources = names
		} else {
			sources = []source.Name{source.Docker}
		}
	}
	for _, name := range sources {
		if !slices.Contains(config.Sources, name) {
			config.Sources = append(config.Sources, name)
		}
	}

	if config.AutoRebootWindow != "" {
		window, err := reboot.ParseWindow(config.AutoRebootWindow)
		if err != nil {
			return Config{}, err
		}
		config.Window = window
	}
	// --help and --version answer whatever else the line says.
	if !config.Help && !config.Version && !slices.Contains(Commands, config.Command) {
		return Config{}, fmt.Errorf("'%s' is not a command", config.Command)
	}
	return config, nil
}

func sourceNames(value, flag string) ([]source.Name, error) {
	var names []source.Name
	for part := range strings.SplitSeq(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, err := source.ParseName(part)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", flag, err)
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("%s needs a source name (%s)", flag, joinNames(source.Names))
	}
	return names, nil
}

func joinNames(names []source.Name) string {
	parts := make([]string, len(names))
	for i, name := range names {
		parts[i] = string(name)
	}
	return strings.Join(parts, ", ")
}

// positiveSeconds is a whole number of seconds, more than none: a pass or a
// tick every 0s would spin, and "1.5" or "1e2" is more likely a mistake than
// a wish.
func positiveSeconds(value, name string) (time.Duration, error) {
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("%s must be a whole number of seconds, got '%s'", name, value)
	}
	if parsed == 0 {
		return 0, fmt.Errorf("%s must be more than 0", name)
	}
	return time.Duration(parsed) * time.Second, nil
}
