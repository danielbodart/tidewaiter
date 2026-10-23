package cli

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/danielbodart/tidewaiter/internal/daemon"
	"github.com/danielbodart/tidewaiter/internal/reboot"
	"github.com/danielbodart/tidewaiter/internal/source"
)

func parse(t *testing.T, env map[string]string, args ...string) Config {
	t.Helper()
	config, err := Parse(args, env)
	if err != nil {
		t.Fatalf("Parse(%q): %v", args, err)
	}
	return config
}

func refuses(t *testing.T, env map[string]string, want string, args ...string) {
	t.Helper()
	_, err := Parse(args, env)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Parse(%q) = %v, want an error containing %q", args, err, want)
	}
}

func equal[T any](t *testing.T, got, want T) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestDefaultsToTheRunCommand(t *testing.T) {
	equal(t, parse(t, nil).Command, "run")
}

func TestTakesABareCommandWord(t *testing.T) {
	for _, command := range Commands {
		equal(t, parse(t, nil, command).Command, command)
	}
}

func TestAWordThatIsNotACommandIsRefused(t *testing.T) {
	refuses(t, nil, "'update' is not a command", "update")
}

func TestHelpAndVersionAnswerWhateverElseTheLineSays(t *testing.T) {
	equal(t, parse(t, nil, "nonsense", "--help").Help, true)
	equal(t, parse(t, nil, "--version").Version, true)
	equal(t, parse(t, nil, "-h").Help, true)
}

// An overridden entrypoint may pass the binary's own path first.
func TestALeadingPathToTheBinaryIsNotACommand(t *testing.T) {
	equal(t, parse(t, nil, "/usr/local/bin/tidewaiter", "check").Command, "check")
	equal(t, parse(t, nil, "/usr/local/bin/tidewaiter").Command, "run")
}

func TestReadsTheIntervalFlagAndItsShortForm(t *testing.T) {
	equal(t, parse(t, nil, "-d", "60").Daemon.Interval, 60*time.Second)
	equal(t, parse(t, nil, "--interval", "120").Daemon.Interval, 120*time.Second)
}

func TestFallsBackToTheEnvIntervalOverriddenByTheFlag(t *testing.T) {
	env := map[string]string{"TIDEWAITER_INTERVAL": "45"}
	equal(t, parse(t, env).Daemon.Interval, 45*time.Second)
	equal(t, parse(t, env, "-d", "10").Daemon.Interval, 10*time.Second)
}

func TestTheDefaultIntervalsAreTheDaemonsDefaults(t *testing.T) {
	equal(t, parse(t, nil).Daemon, daemon.Defaults)
}

// Stricter than the TypeScript, which took anything Number() would.
func TestAnIntervalIsAWholeNumberOfSecondsMoreThanNone(t *testing.T) {
	for _, bad := range []string{"1.5", "1e2", "-5", "abc", "", " 60"} {
		refuses(t, nil, "--interval must be a whole number of seconds, got '"+bad+"'", "--interval", bad)
	}
	refuses(t, nil, "-d must be more than 0", "-d", "0")
	refuses(t, map[string]string{"TIDEWAITER_INTERVAL": "soon"}, "TIDEWAITER_INTERVAL must be a whole number of seconds, got 'soon'")
}

func TestActivityIntervalWhichMustBeMoreThanNothing(t *testing.T) {
	equal(t, parse(t, nil).Daemon.ActivityInterval, daemon.Defaults.ActivityInterval)
	equal(t, parse(t, nil, "--activity-interval", "5").Daemon.ActivityInterval, 5*time.Second)
	refuses(t, nil, "more than 0", "--activity-interval", "0")
}

func TestReadsTheDockerSocketFromFlagAndEnv(t *testing.T) {
	equal(t, parse(t, nil).Socket, "/var/run/docker.sock")
	equal(t, parse(t, nil, "--docker-socket", "/tmp/d.sock").Socket, "/tmp/d.sock")
	equal(t, parse(t, map[string]string{"DOCKER_SOCKET": "/env.sock"}).Socket, "/env.sock")
}

func TestDryRunAndLabel(t *testing.T) {
	config := parse(t, nil, "-n", "-l", "my.label", "check")
	equal(t, config.DryRun, true)
	equal(t, config.Label, "my.label")
	equal(t, config.Command, "check")
	equal(t, parse(t, nil, "--dry-run", "--label", "x").DryRun, true)
	equal(t, parse(t, nil).Label, "tidewaiter.autoupdate")
}

func TestAnUnknownOptionIsRefused(t *testing.T) {
	refuses(t, nil, "unknown option '--nope'", "--nope")
	// Never --flag=value: one spelling, the one the usage shows.
	refuses(t, nil, "unknown option '--interval=60'", "--interval=60")
}

func TestAFlagMissingItsValueIsRefused(t *testing.T) {
	refuses(t, nil, "--interval needs a value", "--interval")
	refuses(t, nil, "-s needs a value", "-s")
}

func TestDockerIsTheOnlySourceByDefaultSoTheImageBehavesAsItAlwaysHas(t *testing.T) {
	config := parse(t, nil)
	equal(t, config.Sources, []source.Name{source.Docker})
	equal(t, config.StatusFile, "")
	equal(t, config.AutoRebootWindow, "")
}

func TestSourceRepeatsOrTakesACommaSeparatedListEachNamedOnce(t *testing.T) {
	equal(t, parse(t, nil, "--source", "systemd").Sources, []source.Name{source.Systemd})
	equal(t, parse(t, nil, "-s", "systemd", "-s", "docker").Sources, []source.Name{source.Systemd, source.Docker})
	equal(t, parse(t, nil, "--source", "systemd, docker,systemd").Sources, []source.Name{source.Systemd, source.Docker})
}

func TestTidewaiterSourcesOverriddenByTheFlag(t *testing.T) {
	env := map[string]string{"TIDEWAITER_SOURCES": "systemd"}
	equal(t, parse(t, env).Sources, []source.Name{source.Systemd})
	equal(t, parse(t, env, "-s", "docker").Sources, []source.Name{source.Docker})
}

func TestAnUnknownSourceIsRefused(t *testing.T) {
	refuses(t, nil, "--source: 'podman' is not a source (docker, systemd)", "--source", "podman")
	refuses(t, nil, "--source needs a source name (docker, systemd)", "--source", ",")
	refuses(t, map[string]string{"TIDEWAITER_SOURCES": "lxc"}, "TIDEWAITER_SOURCES: 'lxc' is not a source")
}

func TestTheUnitsAndStateDirectoriesFromFlagsAndTheEnvironment(t *testing.T) {
	defaults := parse(t, nil)
	equal(t, defaults.UnitsDir, "/etc/tidewaiter/units")
	equal(t, defaults.StateDir, "/var/lib/tidewaiter")
	env := map[string]string{"TIDEWAITER_UNITS_DIR": "/e"}
	equal(t, parse(t, env, "--units-dir", "/u").UnitsDir, "/u")
	equal(t, parse(t, env).UnitsDir, "/e")
	equal(t, parse(t, nil, "--state-dir", "/s").StateDir, "/s")
	// StateDirectory=a b is passed as a colon-separated list; the first is ours.
	equal(t, parse(t, map[string]string{"STATE_DIRECTORY": "/var/lib/tidewaiter:/var/lib/other"}).StateDir, "/var/lib/tidewaiter")
}

func TestThereIsNoShutdownLockToAskForAnyMore(t *testing.T) {
	refuses(t, nil, "unknown option", "--inhibit")
}

func TestStatusFileAndTheEnvironment(t *testing.T) {
	env := map[string]string{"TIDEWAITER_STATUS_FILE": "/e.json"}
	equal(t, parse(t, nil, "--status-file", "/run/t/s.json").StatusFile, "/run/t/s.json")
	equal(t, parse(t, env).StatusFile, "/e.json")
	equal(t, parse(t, env, "--status-file", "/f.json").StatusFile, "/f.json")
	equal(t, parse(t, nil, "status").Command, "status")
}

func TestAutoRebootWindowCheckedWhenGivenAndTheSystemsItCompares(t *testing.T) {
	config := parse(t, nil, "--auto-reboot-window", "23:00-01:00")
	equal(t, config.AutoRebootWindow, "23:00-01:00")
	equal(t, config.Window, reboot.Window{Start: 23 * 60, End: 60})
	equal(t, config.BootedSystem, "/run/booted-system")
	equal(t, config.CurrentSystem, "/nix/var/nix/profiles/system")
	equal(t, parse(t, map[string]string{"TIDEWAITER_AUTO_REBOOT_WINDOW": "03:00-05:00"}).AutoRebootWindow, "03:00-05:00")
	refuses(t, nil, "not a window", "--auto-reboot-window", "3am")
	refuses(t, map[string]string{"TIDEWAITER_AUTO_REBOOT_WINDOW": "later"}, "not a window")
	systems := parse(t, nil, "--booted-system", "/b", "--current-system", "/c")
	equal(t, []string{systems.BootedSystem, systems.CurrentSystem}, []string{"/b", "/c"})
}

func TestTheUsageNamesEveryOptionAndEnvironmentVariable(t *testing.T) {
	for _, mention := range []string{
		"-s, --source", "-d, --interval", "-n, --dry-run", "--status-file", "--activity-interval",
		"--auto-reboot-window", "--booted-system", "--current-system", "--version", "-h, --help",
		"-l, --label", "--docker-socket", "--units-dir", "--state-dir",
		"TIDEWAITER_SOURCES", "TIDEWAITER_INTERVAL", "TIDEWAITER_STATUS_FILE",
		"TIDEWAITER_AUTO_REBOOT_WINDOW", "DOCKER_SOCKET", "TIDEWAITER_UNITS_DIR", "STATE_DIRECTORY",
		"(default 300)", "(default 15)", "(default tidewaiter.autoupdate)", "/run/tidewaiter/status.json",
	} {
		if !strings.Contains(Usage, mention) {
			t.Errorf("the usage does not mention %q", mention)
		}
	}
}
