package cli

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/danielbodart/gokit/await"
	"github.com/danielbodart/gokit/clock"
	"github.com/danielbodart/gokit/httpx"
	"github.com/danielbodart/gokit/sockdiag"
	"github.com/danielbodart/tidewaiter/internal/fakes"
	"github.com/danielbodart/tidewaiter/internal/reboot"
	"github.com/danielbodart/tidewaiter/internal/status"
	"github.com/danielbodart/tidewaiter/internal/systemd"
	"github.com/danielbodart/tidewaiter/internal/version"
)

// world is everything outside Tidewaiter, in memory or in a temporary
// directory: a Docker, a registry that must never be reached, systemd, Nix,
// the kernel's tables and the signals the process is sent. Main runs against
// it exactly as it would against the real ones, down to the bytes on stdout
// and stderr.
type world struct {
	t         *testing.T
	docker    *fakeDocker
	bus       *fakeBus
	nix       *fakeNix
	conntrack *fakes.Conntrack
	boot      *fakes.BootCheck
	rebooter  *fakes.Rebooter
	clock     *clock.Stopped
	signals   chan os.Signal
	stdout    *output
	stderr    *output
	env       map[string]string
	dir       string

	mu        sync.Mutex
	busErr    error
	busOpened int
	registry  int
	// The User-Agent of every registry request.
	agents   []string
	notified []string
}

func newWorld(t *testing.T) *world {
	return &world{
		t:         t,
		docker:    newFakeDocker(),
		bus:       &fakeBus{units: map[string]systemd.UnitState{}},
		nix:       &fakeNix{},
		conntrack: &fakes.Conntrack{},
		boot:      &fakes.BootCheck{},
		rebooter:  &fakes.Rebooter{},
		clock:     clock.NewStopped(time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)),
		signals:   make(chan os.Signal, 2),
		stdout:    &output{},
		stderr:    &output{},
		env:       map[string]string{},
		dir:       t.TempDir(),
	}
}

// notifications is what systemd has been told, one sd_notify each.
func (w *world) notifications() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.notified)
}

func (w *world) path(name string) string { return filepath.Join(w.dir, name) }

func (w *world) container(args ...string) *Container {
	// Never nil, which Wire would read as "the test binary's own arguments".
	args = append([]string{}, args...)
	return &Container{
		Args:   args,
		Env:    w.env,
		Stdout: w.stdout,
		Stderr: w.stderr,
		Clock:  w.clock,
		Notify: func(states ...string) error {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.notified = append(w.notified, strings.Join(states, "\n"))
			return nil
		},
		DockerTransport: func(socket string) http.RoundTripper {
			if socket != "/var/run/docker.sock" && socket != w.env["EXPECT_SOCKET"] {
				w.t.Errorf("Docker reached at %s", socket)
			}
			return httpx.HandlerTransport(w.docker)
		},
		// A refusal, not a transport error, which would be retried on the
		// stopped clock.
		RegistryTransport: httpx.RoundTripperFunc(func(r *http.Request) (*http.Response, error) {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.registry++
			w.agents = append(w.agents, r.Header.Get("User-Agent"))
			return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}, Body: http.NoBody, Request: r}, nil
		}),
		DockerConfig: func() ([]byte, error) { return nil, os.ErrNotExist },
		Bus: func(map[string]string) (systemd.Bus, error) {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.busOpened++
			if w.busErr != nil {
				return nil, w.busErr
			}
			return w.bus, nil
		},
		Rebooter: func(map[string]string) (reboot.Rebooter, error) {
			w.mu.Lock()
			defer w.mu.Unlock()
			if w.busErr != nil {
				return nil, w.busErr
			}
			return w.rebooter, nil
		},
		BootCheck:  func(string, string) reboot.BootCheck { return w.boot },
		Nix:        w.nix,
		RuntimeDir: w.path("run-systemd"),
		Conntrack:  w.conntrack,
		Netns:      &fakes.Netns{},
		Sockets:    sockdiag.ListerFunc(func(context.Context) ([]sockdiag.Listener, error) { return nil, nil }),
		TCP:        &fakes.Prober{},
		Signals:    func() (<-chan os.Signal, func()) { return w.signals, func() {} },
	}
}

// main runs a command to the end.
func (w *world) main(args ...string) int {
	return Main(context.Background(), w.container(args...))
}

// start runs a command that keeps going until it is sent a signal.
func (w *world) start(args ...string) <-chan int {
	code := make(chan int, 1)
	go func() { code <- Main(context.Background(), w.container(args...)) }()
	return code
}

func (w *world) exited(code <-chan int) int {
	w.t.Helper()
	select {
	case c := <-code:
		return c
	case <-time.After(5 * time.Second):
		w.t.Fatal("Main did not return")
		return -1
	}
}

// unit declares a managed unit in a units directory of the world's own.
func (w *world) unit(file, text string) string {
	w.t.Helper()
	dir := w.path("units")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte(text), 0o644); err != nil {
		w.t.Fatal(err)
	}
	return dir
}

func contains(t *testing.T, text, want string) {
	t.Helper()
	if !strings.Contains(text, want) {
		t.Fatalf("%q does not contain %q", text, want)
	}
}

func TestVersionPrintsOnlyTheVersion(t *testing.T) {
	w := newWorld(t)
	equal(t, w.main("--version"), 0)
	equal(t, w.stdout.String(), version.Version+"\n")
	equal(t, w.stderr.String(), "")
}

func TestHelpPrintsTheUsage(t *testing.T) {
	w := newWorld(t)
	equal(t, w.main("--help"), 0)
	equal(t, w.stdout.String(), Usage+"\n")
}

func TestACommandLineThatDoesNotParseSaysWhyThenTheUsageAndExitsTwo(t *testing.T) {
	w := newWorld(t)
	equal(t, w.main("--nope"), 2)
	equal(t, w.stdout.String(), "")
	equal(t, w.stderr.String(), "Error: unknown option '--nope'\n\n"+Usage+"\n")
}

func TestABadRebootWindowStopsTheDaemonAtStartNotAt3am(t *testing.T) {
	w := newWorld(t)
	w.env["TIDEWAITER_AUTO_REBOOT_WINDOW"] = "3am"
	equal(t, w.main(), 2)
	contains(t, w.stderr.String(), "'3am' is not a window")
	equal(t, w.docker.listings(), []string(nil))
}

func TestAnUnknownCommandIsRefusedBeforeAnythingIsReached(t *testing.T) {
	w := newWorld(t)
	equal(t, w.main("update"), 2)
	contains(t, w.stderr.String(), "Error: 'update' is not a command")
	equal(t, w.docker.listings(), []string(nil))
}

func TestStatusWithNoDaemonExitsTwoWithoutTheBanner(t *testing.T) {
	w := newWorld(t)
	file := w.path("status.json")
	equal(t, w.main("status", "--status-file", file), status.Unknown)
	equal(t, w.stdout.String(), "No status at "+file+": is Tidewaiter running, with --status-file?\n")
}

func TestStatusReadsTheEnvironmentsFile(t *testing.T) {
	w := newWorld(t)
	w.env["TIDEWAITER_STATUS_FILE"] = w.path("env.json")
	equal(t, w.main("status"), status.Unknown)
	contains(t, w.stdout.String(), w.path("env.json"))
}

func TestStatusExitsZeroIdleAndOneBusy(t *testing.T) {
	w := newWorld(t)
	file := w.path("status.json")
	written := status.Status{Updated: w.clock.Now(), StaleAfter: 60, Workloads: []status.Workload{{Source: "docker", Workload: "web", Settled: true}}}
	if err := (status.File{Path: file}).Write(written); err != nil {
		t.Fatal(err)
	}
	equal(t, w.main("status", "--status-file", file), status.Idle)
	contains(t, w.stdout.String(), "web")

	written.Busy = []string{"web"}
	written.Workloads[0].Busy = true
	if err := (status.File{Path: file}).Write(written); err != nil {
		t.Fatal(err)
	}
	equal(t, w.main("status", "--status-file", file), status.Busy)
}

// Not 1, which a script would read as "something is busy".
func TestAStatusThatCannotBeReadIsCannotTell(t *testing.T) {
	w := newWorld(t)
	file := w.path("status.json")
	if err := os.WriteFile(file, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	equal(t, w.main("status", "--status-file", file), status.Unknown)
	contains(t, w.stderr.String(), "Error: "+file)
}

func TestListShowsNoOptedInContainersLookingOnlyForItsLabel(t *testing.T) {
	w := newWorld(t)
	equal(t, w.main("list", "-l", "tw-e2e.autoupdate"), 0)
	equal(t, w.stdout.Lines(), []string{"Tidewaiter " + version.Version, "No opted-in containers."})
	equal(t, w.docker.listings(), []string{"tw-e2e.autoupdate"})
	equal(t, w.busOpened, 0)
}

func TestListShowsEachOptedInContainer(t *testing.T) {
	w := newWorld(t)
	w.docker.run("web", "app:latest", map[string]string{"tidewaiter.autoupdate": "local"})
	w.docker.images["app:latest"] = "sha256:" + strings.Repeat("1", 64)
	equal(t, w.main("list"), 0)
	lines := w.stdout.Lines()
	equal(t, len(lines), 2)
	contains(t, lines[1], "web  app:latest  [")
	contains(t, lines[1], "autoupdate=local")
	// A local policy never asks a registry.
	equal(t, w.registry, 0)
}

func TestDockerAndTheRegistriesAreToldWhoIsAsking(t *testing.T) {
	w := newWorld(t)
	w.docker.run("web", "app:latest", map[string]string{"tidewaiter.autoupdate": "registry"})
	w.main("check")
	agent := "tidewaiter/" + version.Version
	equal(t, w.agents, []string{agent})
	if agents := w.docker.userAgents(); len(agents) == 0 || slices.ContainsFunc(agents, func(a string) bool { return a != agent }) {
		t.Fatalf("Docker was told %q", agents)
	}
}

func TestListShowsNoManagedUnitsWhenSystemdIsTheOnlySource(t *testing.T) {
	w := newWorld(t)
	dir := w.unit(".keep", "")
	equal(t, w.main("list", "--source", "systemd", "--units-dir", dir), 0)
	equal(t, w.stdout.Lines()[1:], []string{"No managed units."})
	equal(t, w.docker.listings(), []string(nil))
}

func TestListSaysNothingIsOptedInAcrossBothSources(t *testing.T) {
	w := newWorld(t)
	dir := w.unit(".keep", "")
	equal(t, w.main("list", "-s", "docker,systemd", "--units-dir", dir), 0)
	equal(t, w.stdout.Lines()[1:], []string{"Nothing is opted in."})
}

func TestListShowsADeclaredUnitAndWhereItsProfileIs(t *testing.T) {
	w := newWorld(t)
	profile := w.path("profiles/game")
	dir := w.unit("game.json", `{"unit": "game", "profile": "`+profile+`", "desired": "/nix/store/abc-game"}`)
	w.env["TIDEWAITER_UNITS_DIR"] = dir
	w.env["STATE_DIRECTORY"] = w.path("state")
	equal(t, w.main("list", "-s", "systemd"), 0)
	lines := w.stdout.Lines()
	equal(t, len(lines), 2)
	contains(t, lines[1], "game.service  "+profile+" -> nothing  [no profile yet]  desired=/nix/store/abc-game")
}

func TestASystemBusThatCannotBeUsedFailsTheCommand(t *testing.T) {
	w := newWorld(t)
	w.busErr = errors.New("cannot use DBUS_SYSTEM_BUS_ADDRESS 'tcp:host=x': only unix: addresses are supported")
	equal(t, w.main("list", "-s", "systemd"), 1)
	equal(t, w.stderr.String(), "Error: cannot use DBUS_SYSTEM_BUS_ADDRESS 'tcp:host=x': only unix: addresses are supported\n")
}

func TestCheckRunsOnePassSayingWhatItRunsWithAndWritesNoStatus(t *testing.T) {
	w := newWorld(t)
	file := w.path("status.json")
	equal(t, w.main("check", "-n", "--status-file", file, "-l", "tw-e2e.autoupdate"), 0)
	equal(t, w.stdout.Lines()[:3], []string{"Tidewaiter " + version.Version, "Sources: docker", "Dry run - nothing will be changed"})
	equal(t, w.docker.listings(), []string{"tw-e2e.autoupdate"})
	equal(t, w.conntrack.Calls(), 1)
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("check wrote a status: %v", err)
	}
}

func TestACheckWhosePassCouldNotSeeItsContainersExitsOne(t *testing.T) {
	w := newWorld(t)
	w.docker.failure = "the daemon is restarting"
	equal(t, w.main("check"), 1)
	contains(t, w.stderr.String(), "Error: ")
	contains(t, w.stderr.String(), "the daemon is restarting")
}

func TestRunWritesTheStatusUntilASignalThenClearsItAndExitsZero(t *testing.T) {
	w := newWorld(t)
	file := w.path("status.json")
	code := w.start("--status-file", file, "-l", "tw-e2e.autoupdate")
	await.Until(t, "the first status", func() bool { _, err := os.Stat(file); return err == nil })
	await.Until(t, "the loop to wait", func() bool { return strings.Contains(w.stdout.String(), "Reconciling every 300s...") })

	w.signals <- syscall.SIGTERM
	equal(t, w.exited(code), 0)
	contains(t, w.stdout.String(), "\nReceived SIGTERM, shutting down...\n")
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the status outlived the daemon: %v", err)
	}
}

// Ready before the first pass, so `systemctl start` never waits on a swap;
// then what it is doing; and stopping, once it is told to.
func TestRunTellsSystemdItIsReadyWhatItIsDoingAndThatItIsStopping(t *testing.T) {
	w := newWorld(t)
	code := w.start("-l", "tw-e2e.autoupdate")
	await.Until(t, "the loop to wait", func() bool { return strings.Contains(w.stdout.String(), "Reconciling every 300s...") })
	w.signals <- syscall.SIGTERM
	equal(t, w.exited(code), 0)
	equal(t, w.notifications(), []string{
		"READY=1\nSTATUS=Starting the first pass",
		"STATUS=Idle: No containers to manage",
		"STOPPING=1\nSTATUS=Stopping",
	})
}

func TestOnlyRunTellsSystemdAnything(t *testing.T) {
	w := newWorld(t)
	equal(t, w.main("check"), 0)
	equal(t, w.notifications(), []string(nil))
}

// What the TypeScript did with its AbortSignal: a swap is never abandoned.
func TestASignalDuringAPassLetsThePassFinishFirst(t *testing.T) {
	w := newWorld(t)
	release := make(chan struct{})
	w.docker.hold = release
	code := w.start()
	await.Until(t, "the pass to start", func() bool { return len(w.docker.listings()) == 1 })

	w.signals <- syscall.SIGINT
	await.Until(t, "the signal to be heard", func() bool { return strings.Contains(w.stdout.String(), "Received SIGINT") })
	// A second Ctrl-C is said, not obeyed.
	w.signals <- syscall.SIGINT
	await.Until(t, "the second signal to be heard", func() bool { return strings.Count(w.stdout.String(), "Received SIGINT") == 2 })
	select {
	case <-code:
		t.Fatal("Main returned with a pass still in flight")
	case <-time.After(20 * time.Millisecond):
	}

	close(release)
	equal(t, w.exited(code), 0)
}

func TestRunWithARebootWindowSaysSoAndRebootsWhenNeededAndIdle(t *testing.T) {
	w := newWorld(t)
	w.boot.Need("the kernel changed")
	code := w.start("--auto-reboot-window", "11:00-13:00", "-l", "tw-e2e.autoupdate")
	await.Until(t, "the reboot", func() bool { return w.rebooter.Reboots() == 1 })
	contains(t, w.stdout.String(), "Rebooting automatically when needed, between 11:00-13:00, once nothing is in use\n")
	w.signals <- syscall.SIGTERM
	equal(t, w.exited(code), 0)
}

func TestARebooterThatCannotBeReachedStopsRunAtStart(t *testing.T) {
	w := newWorld(t)
	w.busErr = errors.New("no bus")
	equal(t, w.main("--auto-reboot-window", "03:00-05:00"), 1)
	equal(t, w.stderr.String(), "Error: no bus\n")
	equal(t, w.docker.listings(), []string(nil))
}

func TestOnlyRunRebootsNeverCheck(t *testing.T) {
	w := newWorld(t)
	w.boot.Need("the kernel changed")
	equal(t, w.main("check", "--auto-reboot-window", "11:00-13:00"), 0)
	equal(t, w.rebooter.Reboots(), 0)
	if strings.Contains(w.stdout.String(), "Rebooting automatically") {
		t.Fatal("check promised a reboot")
	}
}

func TestPrepareCreatesTheMissingProfileAndDropInThenReloadsOnce(t *testing.T) {
	w := newWorld(t)
	profile := w.path("profiles/game")
	dir := w.unit("game.json", `{"unit": "game", "profile": "`+profile+`", "desired": "/nix/store/abc-game", "bindClosure": true}`)
	equal(t, w.main("--units-dir", dir, "prepare"), 0)
	equal(t, w.stdout.Lines()[len(w.stdout.Lines())-1], "Prepared "+dir+": 1 profile(s) created, 1 closure drop-in(s) written, 0 failed")
	equal(t, w.nix.ran(), []string{
		"nix-env --profile " + profile + " --set /nix/store/abc-game",
		"nix-store --query --requisites /nix/store/abc-game",
	})
	dropIn, err := os.ReadFile(filepath.Join(w.path("run-systemd"), "game.service.d", "50-tidewaiter-closure.conf"))
	if err != nil {
		t.Fatal(err)
	}
	contains(t, string(dropIn), "BindReadOnlyPaths=/nix/store/abc-game")
	equal(t, w.bus.reloads, 1)
	equal(t, w.bus.closed, true)
}

func TestPrepareExitsOneWhenAUnitFails(t *testing.T) {
	w := newWorld(t)
	profile := w.path("profiles/game")
	dir := w.unit("game.json", `{"unit": "game", "profile": "`+profile+`", "desired": "/nix/store/abc-game", "bindClosure": true}`)
	// The runtime directory is a file, so no drop-in can be written under it.
	if err := os.WriteFile(w.path("run-systemd"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	equal(t, w.main("--units-dir", dir, "prepare"), 1)
	contains(t, w.stderr.String(), "game.service: could not prepare it")
	contains(t, w.stdout.String(), "0 closure drop-in(s) written, 1 failed")
}

func TestPrepareFailsWhenTheUnitsDirectoryCannotBeRead(t *testing.T) {
	w := newWorld(t)
	// A file where the directory should be: one that is not there declares
	// nothing.
	if err := os.WriteFile(w.path("units"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	equal(t, w.main("--units-dir", w.path("units"), "prepare"), 1)
	contains(t, w.stderr.String(), "Error: ")
}

func TestTheSystemdSourceKeepsItsPinsInTheStateDirectory(t *testing.T) {
	w := newWorld(t)
	profile := w.path("profiles/game")
	dir := w.unit("game.json", `{"unit": "game", "profile": "`+profile+`", "desired": "/nix/store/abc-game"}`)
	state := w.path("state")
	// A pin an existing install wrote, in the TypeScript's own format.
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "systemd.json"), []byte(`{"game.service":{"pinned":"/nix/store/abc-game"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	equal(t, w.main("list", "-s", "systemd", "--units-dir", dir, "--state-dir", state), 0)
	contains(t, w.stdout.String(), "[pinned out]")
}

func TestRunOverBothSourcesInTheOrderGiven(t *testing.T) {
	w := newWorld(t)
	dir := w.unit(".keep", "")
	code := w.start("-s", "systemd,docker", "--units-dir", dir, "-d", "60")
	await.Until(t, "the loop to wait", func() bool { return strings.Contains(w.stdout.String(), "Reconciling every 60s...") })
	contains(t, w.stdout.String(), "Sources: systemd, docker\n")
	w.signals <- syscall.SIGTERM
	equal(t, w.exited(code), 0)
	equal(t, w.bus.closed, true)
	if !slices.Contains(w.stdout.Lines(), "Tidewaiter "+version.Version) {
		t.Fatal("no banner")
	}
}
