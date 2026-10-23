package docker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielbodart/gokit/httpx"
	"github.com/danielbodart/tidewaiter/internal/fakes"
	"github.com/danielbodart/tidewaiter/internal/model"
)

func fixture(t *testing.T, name string) Inspected {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var raw Inspected
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestRunningReadsIdPidConfigIdAndHealthOffAnInspect(t *testing.T) {
	running := fixture(t, "inspect-nginx.json").Running()
	// .Image is the config id, kept to look up the running image's manifest
	// digest by - never compared with a registry digest itself.
	if running.ID != "abc123def456" || running.PID != 4242 || running.ImageID != "sha256:aaaa1111" || running.Health != model.Healthy {
		t.Errorf("got %+v", running)
	}
}

func TestRunningMapsAnUnknownOrAbsentHealthToNone(t *testing.T) {
	raw := fixture(t, "inspect-nginx.json")
	raw.State.Health = nil
	if health := raw.Running().Health; health != model.NoHealth {
		t.Errorf("got %q", health)
	}
	raw.State.Health = &struct{ Status string }{"sickly"}
	if health := raw.Running().Health; health != model.NoHealth {
		t.Errorf("got %q", health)
	}
}

func TestRunningIsUpOnlyWhenRunningAndNotRestarting(t *testing.T) {
	raw := fixture(t, "inspect-nginx.json")
	for _, c := range []struct {
		running, restarting, up bool
	}{{true, false, true}, {true, true, false}, {false, false, false}} {
		raw.State.Running, raw.State.Restarting = c.running, c.restarting
		if got := raw.Running().Running; got != c.up {
			t.Errorf("running %v restarting %v: got %v", c.running, c.restarting, got)
		}
	}
}

func TestSpecStripsTheLeadingSlashFromTheName(t *testing.T) {
	if name := fixture(t, "inspect-nginx.json").Spec().Name; name != "web" {
		t.Errorf("got %q", name)
	}
}

func TestSpecReadsPublishedPortsDeduplicatingTheV4V6Pair(t *testing.T) {
	published := fixture(t, "inspect-nginx.json").Spec().Published()
	if want := []model.PublishedPort{port(8080, 80, model.TCP)}; !reflect.DeepEqual(published, want) {
		t.Errorf("got %v", published)
	}
}

func TestSpecSkipsABindingWithNoHostPortOrAnUnknownProtocol(t *testing.T) {
	raw := fixture(t, "inspect-nginx.json")
	raw.HostConfig.PortBindings = map[string][]struct{ HostIP, HostPort string }{
		"80/tcp":   {{HostPort: ""}},
		"90/sctp":  {{HostPort: "90"}},
		"53/udp":   {{HostPort: "5353"}},
		"x/tcp":    {{HostPort: "1"}},
		"100/tcp":  {{HostPort: "1e2"}},
		"9000/tcp": nil,
	}
	if want := []model.PublishedPort{port(5353, 53, model.UDP)}; !reflect.DeepEqual(raw.Spec().Published(), want) {
		t.Errorf("got %v", raw.Spec().Published())
	}
}

func TestSpecReadsABindMount(t *testing.T) {
	mounts := fixture(t, "inspect-nginx.json").Spec().Mounts
	want := []model.Mount{{Type: model.Bind, Source: "/srv/site", Target: "/usr/share/nginx/html", ReadOnly: true}}
	if !reflect.DeepEqual(mounts, want) {
		t.Errorf("got %+v", mounts)
	}
}

func TestSpecReattachesANamedVolumeByNameAndMarksAnAnonymousOne(t *testing.T) {
	named := fixture(t, "inspect-multinet.json").Spec().Mounts
	if want := []model.Mount{{Type: model.Volume, Source: "api-data", Target: "/data"}}; !reflect.DeepEqual(named, want) {
		t.Errorf("got %+v", named)
	}
	anonymous := fixture(t, "inspect-anonvol.json").Spec().Mounts
	if len(anonymous) != 1 || !anonymous[0].Anonymous || anonymous[0].Target != "/var/lib/legacy" {
		t.Errorf("got %+v", anonymous)
	}
}

func TestSpecReadsNetworksWithAliasesAndStaticAddressesItsOwnFirst(t *testing.T) {
	networks := fixture(t, "inspect-multinet.json").Spec().Networks
	want := []model.NetworkAttachment{
		// frontend is the NetworkMode, so first (and the one a create attaches),
		// though Docker writes the map in name order. No static pin, but a
		// runtime IP - what a client reaches, and what port-connect probes.
		{Name: "frontend", Aliases: []string{"api"}, IPAddress: "172.27.0.9"},
		// backend: statically pinned.
		{Name: "backend", Aliases: []string{"api-internal"}, IPv4Address: "172.28.0.5", IPAddress: "172.28.0.5"},
	}
	if !reflect.DeepEqual(networks, want) {
		t.Errorf("got %+v", networks)
	}
}

func TestSpecReadsTheRestOfWhatARecreateNeeds(t *testing.T) {
	spec := fixture(t, "inspect-nginx.json").Spec()
	fifteen := 15
	want := model.ContainerSpec{
		Name: "web", Image: "nginx:1.27",
		Env:        []string{"PATH=/usr/local/sbin:/usr/local/bin", "NGINX_VERSION=1.27"},
		Cmd:        []string{"nginx", "-g", "daemon off;"},
		Entrypoint: []string{"/docker-entrypoint.sh"},
		Labels:     map[string]string{"tidewaiter.autoupdate": "registry", "com.docker.compose.project": "site"},
		Mounts:     spec.Mounts, Bindings: spec.Bindings, Networks: spec.Networks,
		Exposed:       []string{"80/tcp"},
		NetworkMode:   "bridge",
		RestartPolicy: &model.RestartPolicy{Name: "unless-stopped"},
		CapAdd:        []string{}, CapDrop: []string{}, Devices: []model.Device{}, ExtraHosts: []string{}, Sysctls: map[string]string{},
		StopSignal:  "SIGQUIT",
		StopTimeout: &fifteen,
		Healthcheck: &model.Healthcheck{
			Test:     []string{"CMD-SHELL", "curl -f http://localhost/ || exit 1"},
			Interval: 30 * time.Second, Timeout: 3 * time.Second, Retries: 3,
		},
	}
	if !reflect.DeepEqual(spec, want) {
		t.Errorf("got  %+v\nwant %+v", spec, want)
	}
}

func TestSpecDropsANoRestartPolicy(t *testing.T) {
	raw := fixture(t, "inspect-nginx.json")
	raw.HostConfig.RestartPolicy.Name = "no"
	if policy := raw.Spec().RestartPolicy; policy != nil {
		t.Errorf("got %+v", policy)
	}
}

// recording is a client over a handler, with every request it was asked.
func recording(handler http.HandlerFunc) (*Client, func() []*http.Request) {
	var mu sync.Mutex
	var requests []*http.Request
	client := NewClient(httpx.StreamingHandlerTransport(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r)
		mu.Unlock()
		handler(w, r)
	})))
	return client, func() []*http.Request {
		mu.Lock()
		defer mu.Unlock()
		return requests
	}
}

func replying(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
}

func TestImageDigestReadsTheFirstRepoDigestWithoutItsRepository(t *testing.T) {
	client, requests := recording(replying(200, `{"RepoDigests":["ghcr.io/me/app@sha256:beef","ghcr.io/me/app@sha256:older"]}`))
	digest, err := client.ImageDigest(context.Background(), "ghcr.io/me/app:1")
	if err != nil || digest != "sha256:beef" {
		t.Errorf("got %q, %v", digest, err)
	}
	// One path segment, however many slashes the ref has.
	if path := requests()[0].URL.EscapedPath(); path != "/v1.41/images/ghcr.io%2Fme%2Fapp:1/json" {
		t.Errorf("asked %s", path)
	}
}

func TestImageDigestIsEmptyForAnImageThatIsNotHereOrWasNeverPulled(t *testing.T) {
	for _, handler := range []http.HandlerFunc{replying(404, `{"message":"No such image"}`), replying(200, `{"RepoDigests":[]}`)} {
		client, _ := recording(handler)
		if digest, err := client.ImageDigest(context.Background(), "ghost:latest"); err != nil || digest != "" {
			t.Errorf("got %q, %v", digest, err)
		}
	}
}

func TestNetworkDriverReadsTheDriverAndAsksForTheNetworkByName(t *testing.T) {
	client, requests := recording(replying(200, `{"Driver":"macvlan"}`))
	if driver, err := client.NetworkDriver(context.Background(), "pub-net"); err != nil || driver != "macvlan" {
		t.Errorf("got %q, %v", driver, err)
	}
	if path := requests()[0].URL.Path; path != "/v1.41/networks/pub-net" {
		t.Errorf("asked %s", path)
	}
}

func TestNetworkDriverIsEmptyForANetworkThatIsGone(t *testing.T) {
	client, _ := recording(replying(404, "no such network"))
	if driver, err := client.NetworkDriver(context.Background(), "ghost-net"); err != nil || driver != "" {
		t.Errorf("got %q, %v", driver, err)
	}
}

func TestPullSendsXRegistryAuthAndReadsTheWholeStream(t *testing.T) {
	finished := false
	client, requests := recording(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "{\"status\":\"Pulling\"}\n")
		w.(http.Flusher).Flush()
		io.WriteString(w, "{\"status\":\"Done\"}\n")
		finished = true
	})
	if err := client.Pull(context.Background(), "nginx:1.27", "authblob"); err != nil {
		t.Fatal(err)
	}
	request := requests()[0]
	if request.Header.Get("X-Registry-Auth") != "authblob" || request.URL.Query().Get("fromImage") != "nginx:1.27" {
		t.Errorf("asked %v with %v", request.URL, request.Header)
	}
	if !finished {
		t.Error("returned before the pull finished")
	}
}

// Given a bare name, Docker pulls every tag of the repository.
func TestPullOfAnUntaggedImagePullsLatestNotEveryTag(t *testing.T) {
	for ref, asked := range map[string]string{
		"nginx":                           "nginx:latest",
		"localhost:5000/app":              "localhost:5000/app:latest",
		"ghcr.io/owner/app":               "ghcr.io/owner/app:latest",
		"localhost:5000/app:2":            "localhost:5000/app:2",
		"nginx@sha256:" + hex64("a"):      "nginx@sha256:" + hex64("a"),
		"nginx:1.27@sha256:" + hex64("b"): "nginx:1.27@sha256:" + hex64("b"),
	} {
		client, requests := recording(replying(200, "{\"status\":\"Done\"}\n"))
		if err := client.Pull(context.Background(), ref, ""); err != nil {
			t.Fatal(err)
		}
		if got := requests()[0].URL.Query().Get("fromImage"); got != asked {
			t.Errorf("%s: asked for %s, want %s", ref, got, asked)
		}
	}
}

func hex64(digit string) string { return strings.Repeat(digit, 64) }

// Docker says a pull failed inside its 200 stream; that must not read as
// success.
func TestPullFailsWithTheErrorInItsStream(t *testing.T) {
	client, requests := recording(replying(200, "{\"status\":\"Pulling\"}\n{\"errorDetail\":{\"message\":\"manifest unknown\"},\"error\":\"manifest unknown\"}\n"))
	err := client.Pull(context.Background(), "nginx:9", "")
	if err == nil || !strings.Contains(err.Error(), "pull nginx:9") || !strings.Contains(err.Error(), "manifest unknown") {
		t.Errorf("got %v", err)
	}
	if _, sent := requests()[0].Header["X-Registry-Auth"]; sent {
		t.Error("sent an empty credential")
	}
}

func TestStopPassesTheGraceAsT(t *testing.T) {
	client, requests := recording(replying(204, ""))
	if err := client.Stop(context.Background(), "web", 15); err != nil {
		t.Fatal(err)
	}
	if request := requests()[0]; request.URL.Path != "/v1.41/containers/web/stop" || request.URL.Query().Get("t") != "15" {
		t.Errorf("asked %v", request.URL)
	}
}

func TestADockerErrorNamesTheEndpointAndCarriesDockersWords(t *testing.T) {
	client, _ := recording(replying(500, `{"message":"boom"}`))
	err := client.Start(context.Background(), "web")
	if err == nil || !strings.Contains(err.Error(), "/v1.41/containers/web/start") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("got %v", err)
	}
}

func TestRemoveTakesAnonymousVolumesAndForcesOnlyWhenAsked(t *testing.T) {
	client, requests := recording(replying(204, ""))
	client.Remove(context.Background(), "web", false)
	client.Remove(context.Background(), "web", true)
	if got := requests(); got[0].URL.RawQuery != "v=1" || got[1].URL.RawQuery != "force=1&v=1" || got[0].Method != http.MethodDelete {
		t.Errorf("asked %v, %v", got[0].URL, got[1].URL)
	}
}

func TestNetBytesSumsRxAndTxAcrossInterfacesFromOneSnapshot(t *testing.T) {
	client, requests := recording(replying(200, `{"networks":{"eth0":{"rx_bytes":100,"tx_bytes":50},"eth1":{"rx_bytes":5,"tx_bytes":1}}}`))
	bytes, err := client.NetBytes(context.Background(), "web")
	if err != nil || bytes != 156 {
		t.Errorf("got %d, %v", bytes, err)
	}
	if query := requests()[0].URL.Query(); query.Get("stream") != "false" {
		t.Errorf("asked %v", requests()[0].URL)
	}
}

func TestAContainerWithNoNetworksBlockTotalsZero(t *testing.T) {
	for _, body := range []string{`{}`, `{"networks":null}`} {
		var stats Stats
		json.Unmarshal([]byte(body), &stats)
		if stats.NetBytes() != 0 {
			t.Errorf("%s: got %d", body, stats.NetBytes())
		}
	}
}

func TestContainersInspectsEachLabelledContainer(t *testing.T) {
	docker := newFakeDocker()
	docker.add(container("web"), container("other", func(c *model.RunningContainer) { c.Spec.Labels = map[string]string{} }))
	containers, err := NewClient(httpx.HandlerTransport(docker)).Containers(context.Background(), AutoupdateLabel)
	if err != nil || len(containers) != 1 || containers[0].Spec.Name != "web" || containers[0].PID != 4242 {
		t.Errorf("got %+v, %v", containers, err)
	}
}

func TestEventsHandsOverEachEventAsItHappensWithTheHealthVerdictSplitOut(t *testing.T) {
	docker := newFakeDocker()
	client := NewClient(httpx.StreamingHandlerTransport(docker))
	docker.emit(Event{Action: "start", Container: "web"})
	docker.emit(Event{Action: "health_status: unhealthy", Container: "web"})

	var seen []Event
	err := client.Events(context.Background(), AutoupdateLabel, func(event Event) bool {
		seen = append(seen, event)
		return len(seen) == 2
	})
	want := []Event{{Action: "start", Container: "web"}, {Action: "health_status: unhealthy", Container: "web", Status: "unhealthy"}}
	if err != nil || !reflect.DeepEqual(seen, want) {
		t.Errorf("got %+v, %v", seen, err)
	}
	// Stopping let go of the stream.
	fakes.Await(t, "the stream to close", func() bool { return docker.open() == 0 })
}

func TestEventsEndsWithItsContextAndLetsGoOfTheStream(t *testing.T) {
	docker := newFakeDocker()
	client := NewClient(httpx.StreamingHandlerTransport(docker))
	ctx, cancel := context.WithCancel(context.Background())
	ended := make(chan error)
	go func() { ended <- client.Events(ctx, AutoupdateLabel, func(Event) bool { return false }) }()
	fakes.Await(t, "the stream to open", func() bool { return docker.open() == 1 })

	cancel()
	if err := <-ended; err != nil {
		t.Errorf("got %v", err)
	}
	fakes.Await(t, "the stream to close", func() bool { return docker.open() == 0 })
}

func TestEventsReturnsWhenDockerDropsTheStream(t *testing.T) {
	docker := newFakeDocker()
	client := NewClient(httpx.StreamingHandlerTransport(docker))
	ended := make(chan error)
	go func() {
		ended <- client.Events(context.Background(), AutoupdateLabel, func(Event) bool { return false })
	}()
	fakes.Await(t, "the stream to open", func() bool { return docker.open() == 1 })

	docker.dropStreams()
	if err := <-ended; err != nil {
		t.Errorf("got %v", err)
	}
}
