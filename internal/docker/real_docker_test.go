package docker

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/danielbodart/gokit/clock"
	"github.com/danielbodart/gokit/httpx"
	"github.com/danielbodart/tidewaiter/internal/decide"
	"github.com/danielbodart/tidewaiter/internal/detect"
	"github.com/danielbodart/tidewaiter/internal/fakes"
	"github.com/danielbodart/tidewaiter/internal/model"
	"github.com/danielbodart/tidewaiter/internal/source"
)

// These run against a real Docker, which is the only thing that can say the
// create payload is one Docker accepts and the inspect mapping reads what
// Docker really writes. They create containers, so they run only when asked
// to (TIDEWAITER_DOCKER_TESTS=1), and touch nothing of anyone else's: every
// container is named tw-e2e-*, carries only the tw-e2e.autoupdate label, and
// is removed afterwards; images are never pulled, only tagged under
// tw-e2e-app, which is untagged afterwards.
const e2eLabel = "tw-e2e.autoupdate"

func realDocker(t *testing.T, images ...string) *Client {
	t.Helper()
	if os.Getenv("TIDEWAITER_DOCKER_TESTS") != "1" {
		t.Skip("set TIDEWAITER_DOCKER_TESTS=1 to run against the Docker at DOCKER_HOST")
	}
	socket := "/var/run/docker.sock"
	if host, ok := strings.CutPrefix(os.Getenv("DOCKER_HOST"), "unix://"); ok {
		socket = host
	}
	if _, err := os.Stat(socket); err != nil {
		t.Skipf("no Docker socket: %v", err)
	}
	client := NewClient(httpx.UnixSocket(socket))
	for _, image := range images {
		if digest, err := client.ImageDigest(t.Context(), image); err != nil || digest == "" {
			t.Skipf("%s is not here with a registry digest, and is never pulled for a test (%v)", image, err)
		}
	}
	return client
}

// e2eName is a container name of this test's own, removed when it ends.
func e2eName(t *testing.T, client *Client, what string) string {
	t.Helper()
	name := fmt.Sprintf("tw-e2e-%s-%d", what, time.Now().UnixNano())
	t.Cleanup(func() {
		ctx := context.Background()
		for _, each := range []string{name, RollbackName(name)} {
			client.Remove(ctx, each, true)
		}
	})
	return name
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func e2eSpec(name, image string, hostPort int) model.ContainerSpec {
	one := 1
	s := spec(name)
	s.Image = image
	s.Cmd = []string{"sleep", "3600"}
	s.Env = []string{"TW_E2E=1"}
	s.Labels = map[string]string{e2eLabel: "local", DetectorLabel: "none", IdleSamplesLabel: "1", HealthLabel: "docker,uptime", HealthTimeoutLabel: "20"}
	s.Bindings = bound(port(hostPort, 8080, model.TCP))
	s.Exposed = []string{"8080/tcp"}
	s.Mounts = []model.Mount{{Type: model.Tmpfs, Target: "/scratch"}}
	s.RestartPolicy = &model.RestartPolicy{Name: "on-failure", MaximumRetryCount: 2}
	s.CapDrop = []string{"MKNOD"}
	s.ExtraHosts = []string{"db:10.0.0.9"}
	s.WorkingDir = "/tmp"
	s.StopSignal = "SIGTERM"
	s.StopTimeout = &one
	s.Healthcheck = &model.Healthcheck{Test: []string{"CMD", "true"}, Interval: time.Second, Retries: 3}
	return s
}

// Docker reads back what a recreate asked of it as the container that was
// there.
func TestARealRecreateIsFaithful(t *testing.T) {
	client := realDocker(t, "alpine:latest")
	ctx := t.Context()
	name := e2eName(t, client, "faithful")
	if _, err := client.Create(ctx, e2eSpec(name, "alpine:latest", freePort(t))); err != nil {
		t.Fatal(err)
	}
	if err := client.Start(ctx, name); err != nil {
		t.Fatal(err)
	}
	original, err := client.Inspect(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if !original.Running || original.PID == 0 || original.Spec.Labels[e2eLabel] != "local" || original.Spec.Published()[0].ContainerPort != 8080 {
		t.Fatalf("inspected %+v", original)
	}

	// Recreated from what inspect said, under another name and host port.
	copied := e2eName(t, client, "copy")
	again := original.Spec
	again.Name = copied
	again.Bindings = bound(port(freePort(t), 8080, model.TCP))
	if _, err := client.Create(ctx, again); err != nil {
		t.Fatal(err)
	}
	recreated, err := client.Inspect(ctx, copied)
	if err != nil {
		t.Fatal(err)
	}
	// Not started, so it has no runtime address yet.
	want := again
	for i := range want.Networks {
		want.Networks[i].IPAddress = ""
	}
	got := recreated.Spec
	for i := range got.Networks {
		got.Networks[i].IPAddress = ""
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("recreated as\n%+v\nwant\n%+v", got, want)
	}

	if driver, err := client.NetworkDriver(ctx, "bridge"); err != nil || driver != "bridge" {
		t.Errorf("bridge's driver: %q, %v", driver, err)
	}
	if driver, err := client.NetworkDriver(ctx, "tw-e2e-no-such-network"); err != nil || driver != "" {
		t.Errorf("a missing network's driver: %q, %v", driver, err)
	}
	if _, err := client.NetBytes(ctx, name); err != nil {
		t.Errorf("stats: %v", err)
	}
	if digest, err := client.ImageDigest(ctx, "tw-e2e-no-such-image:1"); err != nil || digest != "" {
		t.Errorf("a missing image: %q, %v", digest, err)
	}
}

// swapped is a real container on tw-e2e-app:latest, the tag moved on to
// another image here, and the outcome of the pass that saw it.
type swapped struct {
	client        *Client
	name          string
	moved         model.Digest
	before, after model.RunningContainer
	action        decide.Kind
	log           *fakes.Log
	source        *Source
}

func swapReally(t *testing.T, change func(*model.ContainerSpec)) swapped {
	t.Helper()
	client := realDocker(t, "alpine:latest", "postgres:18")
	ctx := t.Context()
	tag := func(image string) {
		t.Helper()
		request, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://docker/v1.41/images/"+image+"/tag?repo=tw-e2e-app&tag=latest", nil)
		response, err := client.http.Do(request)
		if err == nil {
			err = httpx.Check(response)
			drain(response)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	tag("alpine:latest")
	t.Cleanup(func() { client.RemoveImage(context.Background(), "tw-e2e-app:latest") })

	s := swapped{client: client, name: e2eName(t, client, "swap"), log: &fakes.Log{}}
	created := e2eSpec(s.name, "tw-e2e-app:latest", freePort(t))
	created.Labels[HealthTimeoutLabel] = "4"
	change(&created)
	if _, err := client.Create(ctx, created); err != nil {
		t.Fatal(err)
	}
	if err := client.Start(ctx, s.name); err != nil {
		t.Fatal(err)
	}
	var err error
	if s.before, err = client.Inspect(ctx, s.name); err != nil {
		t.Fatal(err)
	}
	// The tag moves on: a local build or pull, as far as Tidewaiter can tell.
	tag("postgres:18")
	s.moved, _ = client.ImageDigest(ctx, "postgres:18")

	options := DefaultOptions
	options.Label = e2eLabel
	options.HealthPoll = 250 * time.Millisecond
	s.source = NewSource(Deps{Docker: client, Netns: &fakes.Netns{}, TCP: &fakes.Prober{}, Options: options, Log: s.log.Line, Clock: clock.System{}})
	s.action = s.pass(t)
	if s.after, err = client.Inspect(ctx, s.name); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Inspect(ctx, RollbackName(s.name)); err == nil {
		t.Error("the parked container was left behind")
	}
	return s
}

func (s swapped) pass(t *testing.T) decide.Kind {
	t.Helper()
	outcomes, err := s.source.Once(t.Context(), source.Pass{Conntrack: []detect.ConntrackEntry{}, Hold: func(string) {}})
	if err != nil {
		t.Fatal(err)
	}
	for _, outcome := range outcomes {
		if outcome.Workload == s.name {
			return outcome.Action.Kind
		}
	}
	t.Fatalf("%s was not seen: %+v", s.name, outcomes)
	return 0
}

// A whole swap against a real Docker: the container is recreated onto the
// moved tag, health-gated on Docker's own healthcheck and uptime, and
// committed.
func TestARealSwapOntoAMovedTag(t *testing.T) {
	s := swapReally(t, func(spec *model.ContainerSpec) { spec.Bindings = []model.PortBinding{} })
	if s.action != decide.Update || !s.log.Contains(s.name+": updated to "+string(s.moved)+" and it is healthy") {
		t.Fatalf("%v; said:\n%s", s.action, s.log.Text())
	}
	if s.after.ID == s.before.ID || s.after.ImageID == s.before.ImageID || !s.after.Running {
		t.Errorf("before %s on %s, after %s on %s (running %v)", s.before.ID, s.before.ImageID, s.after.ID, s.after.ImageID, s.after.Running)
	}
	if kind := s.pass(t); kind != decide.Keep {
		t.Errorf("then %v", kind)
	}
}

// And one that comes up but never binds the port it publishes: rolled back
// onto the very container that was there, and pinned out.
func TestARealSwapThatDoesNotServeIsRolledBack(t *testing.T) {
	s := swapReally(t, func(*model.ContainerSpec) {})
	if s.action != decide.Update || !s.log.Contains("no published port is bound") || !s.log.Contains("rolled back") {
		t.Fatalf("%v; said:\n%s", s.action, s.log.Text())
	}
	if s.after.ID != s.before.ID || !s.after.Running {
		t.Errorf("before %s, after %s (running %v)", s.before.ID, s.after.ID, s.after.Running)
	}
	if kind := s.pass(t); kind != decide.Pinned {
		t.Errorf("then %v", kind)
	}
}
