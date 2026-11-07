package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danielbodart/gokit/clock"
	"github.com/danielbodart/gokit/httpx"
	"github.com/danielbodart/gokit/logx/logtest"
	"github.com/danielbodart/tidewaiter/internal/fakes"
)

// cappedAt is a transport on which Docker says it speaks no newer than
// version, so a real Docker can be recreated against at an older API.
// A Docker older than version is left to say so, and the test skipped.
func cappedAt(transport http.RoundTripper, version string) http.RoundTripper {
	return httpx.RoundTripperFunc(func(request *http.Request) (*http.Response, error) {
		response, err := transport.RoundTrip(request)
		if err != nil || request.URL.Path != "/version" || response.StatusCode != http.StatusOK {
			return response, err
		}
		var offered map[string]any
		data, _ := io.ReadAll(response.Body)
		response.Body.Close()
		json.Unmarshal(data, &offered)
		if newest, _ := offered["ApiVersion"].(string); compareVersions(newest, version) > 0 {
			offered["ApiVersion"] = version
		}
		data, _ = json.Marshal(offered)
		response.Body = io.NopCloser(bytes.NewReader(data))
		response.ContentLength = int64(len(data))
		return response, nil
	})
}

// call is one Engine API call of a test's own, read into into.
func call(t *testing.T, client *Client, method, path string, body, into any) {
	t.Helper()
	if err := client.quick(t.Context(), method, path, nil, body, into); err != nil {
		t.Fatal(err)
	}
}

// inspectJSON is a container's inspect, whole and untyped.
func inspectJSON(t *testing.T, client *Client, name string) map[string]any {
	t.Helper()
	var inspected map[string]any
	call(t, client, http.MethodGet, "/containers/"+name+"/json", nil, &inspected)
	return inspected
}

// A container made with a broad spread of what `docker run` can set is
// recreated through Tidewaiter's own swap, and inspects afterwards as it did
// before, but for what the overrides change on purpose: what Docker reports
// goes back to it untouched. At the API version each MAC address rule turns
// on, and the newest.
func TestARealRecreatePassesEverythingThrough(t *testing.T) {
	for _, version := range []string{"1.43", apiEndpointMAC, "1.51", apiInspectEndpointMAC, apiConnectMAC, NewestAPI} {
		t.Run("v"+version, func(t *testing.T) { roundTrip(t, version) })
	}
}

func roundTrip(t *testing.T, version string) {
	realDocker(t, "alpine:latest")
	socket := "/var/run/docker.sock"
	if host, ok := strings.CutPrefix(os.Getenv("DOCKER_HOST"), "unix://"); ok {
		socket = host
	}
	client := NewClient(cappedAt(httpx.UnixSocket(socket), version), nil)
	ctx := t.Context()
	if spoken, err := client.apiVersion(ctx); err != nil || spoken != version {
		t.Skipf("Docker will not speak v%s here (%s, %v)", version, spoken, err)
	}
	stamp := time.Now().UnixNano()
	octet := 100 + stamp%100

	// Two networks, one with a subnet for a static address, and a volume.
	front, back, volume := fmt.Sprintf("tw-e2e-front-%d", stamp), fmt.Sprintf("tw-e2e-back-%d", stamp), fmt.Sprintf("tw-e2e-data-%d", stamp)
	call(t, client, http.MethodPost, "/networks/create", map[string]any{
		"Name": front, "Labels": map[string]string{e2eLabel: "local"},
		"IPAM": map[string]any{"Config": []map[string]string{{"Subnet": fmt.Sprintf("10.231.%d.0/24", octet)}}},
	}, nil)
	call(t, client, http.MethodPost, "/networks/create", map[string]any{"Name": back, "Labels": map[string]string{e2eLabel: "local"}}, nil)
	call(t, client, http.MethodPost, "/volumes/create", map[string]any{"Name": volume, "Labels": map[string]string{e2eLabel: "local"}}, nil)
	name := e2eName(t, client, "roundtrip")
	t.Cleanup(func() {
		ctx := context.Background()
		client.Remove(ctx, name, true)
		for _, network := range []string{front, back} {
			client.quick(ctx, http.MethodDelete, "/networks/"+network, nil, nil, nil)
		}
		client.quick(ctx, http.MethodDelete, "/volumes/"+volume, nil, nil, nil)
	})
	bound, long := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(bound, "hello"), []byte("hi"), 0o644)

	const mac = "02:00:0a:e7:00:0a"
	mib := int64(1 << 20)
	mounts := []map[string]any{
		{"Type": "volume", "Source": volume, "Target": "/data", "VolumeOptions": map[string]any{"NoCopy": true}},
		{"Type": "tmpfs", "Target": "/scratch", "TmpfsOptions": map[string]any{"SizeBytes": 16 * mib, "Mode": 0o1777}},
		{"Type": "bind", "Source": long, "Target": "/srv/long", "ReadOnly": true},
	}
	if atLeast(version, "1.48") {
		mounts = append(mounts, map[string]any{"Type": "image", "Source": "alpine:latest", "Target": "/img", "ImageOptions": map[string]any{"Subpath": "etc"}})
	}
	healthcheck := map[string]any{"Test": []string{"CMD", "true"}, "Interval": 2 * time.Second, "Timeout": time.Second, "Retries": 3, "StartPeriod": 3 * time.Second}
	if atLeast(version, "1.44") {
		healthcheck["StartInterval"] = 500 * time.Millisecond
	}
	host := map[string]any{
		"Memory": 64 * mib, "MemoryReservation": 32 * mib, "NanoCpus": 500_000_000, "CpuShares": 512, "PidsLimit": 100,
		"Ulimits":     []map[string]any{{"Name": "nofile", "Soft": 1024, "Hard": 2048}},
		"LogConfig":   map[string]any{"Type": "json-file", "Config": map[string]string{"max-size": "1m", "max-file": "2"}},
		"SecurityOpt": []string{"no-new-privileges"},
		"Dns":         []string{"10.231.0.53"}, "DnsSearch": []string{"example.test"}, "DnsOptions": []string{"ndots:2"},
		"Init": true, "ShmSize": 32 * mib,
		"Tmpfs":         map[string]string{"/run/tw": "size=8m"},
		"Binds":         []string{bound + ":/srv/bind:ro"},
		"Mounts":        mounts,
		"PortBindings":  map[string]any{"8080/tcp": []map[string]string{{"HostIp": "127.0.0.1", "HostPort": ""}}},
		"RestartPolicy": map[string]any{"Name": "on-failure", "MaximumRetryCount": 2},
		"CapAdd":        []string{"NET_ADMIN"}, "CapDrop": []string{"MKNOD"},
		"Devices":     []map[string]string{{"PathOnHost": "/dev/null", "PathInContainer": "/dev/null", "CgroupPermissions": "rwm"}},
		"Sysctls":     map[string]string{"net.ipv4.ip_unprivileged_port_start": "1024"},
		"ExtraHosts":  []string{"db:10.0.0.9"},
		"GroupAdd":    []string{"1234"},
		"OomScoreAdj": 100,
		"NetworkMode": front,
	}
	if atLeast(version, "1.43") {
		host["Annotations"] = map[string]string{"io.example.tw": "e2e"}
	}
	endpoint := map[string]any{"Aliases": []string{"tw-alias"}, "IPAMConfig": map[string]string{"IPv4Address": fmt.Sprintf("10.231.%d.10", octet)}}
	body := map[string]any{
		"Image": "alpine:latest", "Cmd": []string{"sleep", "3600"}, "Env": []string{"TW_E2E=1"},
		"Labels": map[string]string{e2eLabel: "local", "tw-e2e.kind": "roundtrip"},
		"User":   "65534:65534", "WorkingDir": "/tmp", "Domainname": "example.test",
		"StopSignal": "SIGTERM", "StopTimeout": 3,
		"Healthcheck":      healthcheck,
		"ExposedPorts":     map[string]any{"8080/tcp": map[string]any{}},
		"HostConfig":       host,
		"NetworkingConfig": map[string]any{"EndpointsConfig": map[string]any{front: endpoint}},
	}
	if atLeast(version, apiEndpointMAC) {
		endpoint["MacAddress"] = mac
	} else {
		body["MacAddress"] = mac
	}
	if err := client.quick(ctx, http.MethodPost, "/containers/create", map[string][]string{"name": {name}}, body, nil); err != nil {
		t.Fatalf("creating the original: %v", err)
	}
	call(t, client, http.MethodPost, "/networks/"+back+"/connect", map[string]any{"Container": name, "EndpointConfig": map[string]any{"Aliases": []string{"tw-back"}}}, nil)
	if err := client.Start(ctx, name); err != nil {
		t.Fatalf("starting the original: %v", err)
	}
	before, after := recreated(t, client, name)
	// The MAC address asked for on its own network is kept at every version.
	networks := after["NetworkSettings"].(map[string]any)["Networks"].(map[string]any)
	if got := networks[front].(map[string]any)["MacAddress"]; got != mac {
		t.Errorf("v%s: MAC address %v, want %s", version, got, mac)
	}
	// And the one Docker picked on the network connected after is kept from
	// 1.54, the first that applies it there.
	if atLeast(version, apiConnectMAC) {
		was := before["NetworkSettings"].(map[string]any)["Networks"].(map[string]any)[back].(map[string]any)["MacAddress"]
		if got := networks[back].(map[string]any)["MacAddress"]; got != was {
			t.Errorf("v%s: %s's MAC address %v, want %v", version, back, got, was)
		}
	}
}

// recreated is a container's inspect before and after Tidewaiter's own
// swap recreates it onto the same image, failing the test where the two
// differ in anything but what the overrides change on purpose.
func recreated(t *testing.T, client *Client, name string) (before, after map[string]any) {
	t.Helper()
	ctx := t.Context()
	before = inspectJSON(t, client, name)
	running, err := client.Inspect(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	image, _, err := client.Image(ctx, running.ImageID)
	if err != nil {
		t.Fatal(err)
	}
	source := NewSource(Deps{Docker: client, Netns: &fakes.Netns{}, TCP: &fakes.Prober{}, Options: DefaultOptions, Log: &logtest.Logger{}, Clock: clock.System{}})
	plan := PlanSwap(running.Spec, WithoutImageDefaults(running.Spec, image.Defaults), 1)
	if done, err := source.run(ctx, plan.Swap); err != nil {
		source.undo(ctx, name, done)
		t.Fatalf("recreating: %v", err)
	}
	if err := source.steps(ctx, plan.Commit); err != nil {
		t.Fatal(err)
	}
	after = inspectJSON(t, client, name)

	oldID, newID := before["Id"].(string), after["Id"].(string)
	if oldID == newID {
		t.Fatal("not recreated")
	}
	// What the overrides change on purpose: a default hostname, which was the
	// old id and is now the new one, and each endpoint's operational data.
	// And what is not the container's config at all: where Docker mounted an
	// image mount's image this time.
	config := before["Config"].(map[string]any)
	if config["Hostname"] == oldID[:12] && after["Config"].(map[string]any)["Hostname"] != newID[:12] {
		t.Errorf("hostname %v, want %s", after["Config"].(map[string]any)["Hostname"], newID[:12])
	}
	ignored := map[string]bool{}
	for network := range before["NetworkSettings"].(map[string]any)["Networks"].(map[string]any) {
		for _, key := range operational {
			ignored["Networks."+network+"."+key] = true
		}
	}
	for _, mount := range before["Mounts"].([]any) {
		if mount := mount.(map[string]any); mount["Type"] == "image" {
			ignored["Mounts."+fmt.Sprint(mount["Destination"])+".Source"] = true
		}
	}
	var unexpected []string
	for _, diff := range differences(comparable(before, oldID), comparable(after, newID)) {
		path := strings.SplitN(diff, " ", 2)[0]
		if !ignored[path] && !ignoredPrefix(path, ignored) {
			unexpected = append(unexpected, diff)
		}
	}
	if len(unexpected) > 0 {
		t.Errorf("v%s: recreated differently:\n%s", running.Spec.Inspected.APIVersion, strings.Join(unexpected, "\n"))
	}
	return before, after
}

// comparable is the parts of an inspect a recreate is answerable for -
// Config, HostConfig, each network's endpoint settings and the mounts - as
// paths to values, with the container's own id taken out wherever it
// appears, so two containers compare.
func comparable(inspected map[string]any, id string) map[string]string {
	flat := map[string]string{}
	var walk func(path string, value any)
	walk = func(path string, value any) {
		switch value := value.(type) {
		case map[string]any:
			for key, inner := range value {
				walk(path+"."+key, inner)
			}
		case []any:
			if path == "Mounts" {
				for _, mount := range value {
					walk(path+"."+fmt.Sprint(mount.(map[string]any)["Destination"]), mount)
				}
				return
			}
			data, _ := json.Marshal(value)
			flat[strings.TrimPrefix(path, ".")] = strings.ReplaceAll(strings.ReplaceAll(string(data), id, "<id>"), id[:12], "<id>")
		default:
			data, _ := json.Marshal(value)
			text := strings.ReplaceAll(string(data), id[:12], "<id>")
			flat[strings.TrimPrefix(path, ".")] = strings.ReplaceAll(text, id, "<id>")
		}
	}
	walk("Config", inspected["Config"])
	walk("HostConfig", inspected["HostConfig"])
	walk("Networks", inspected["NetworkSettings"].(map[string]any)["Networks"])
	walk("Mounts", inspected["Mounts"])
	return flat
}

// differences is each path whose value differs, as "path: before -> after".
func differences(before, after map[string]string) []string {
	var found []string
	for _, path := range slices.Sorted(maps.Keys(maps.Collect(func(yield func(string, bool) bool) {
		for path := range before {
			yield(path, true)
		}
		for path := range after {
			yield(path, true)
		}
	}))) {
		if before[path] != after[path] {
			found = append(found, fmt.Sprintf("%s %s -> %s", path, cut(before[path]), cut(after[path])))
		}
	}
	return found
}

func cut(text string) string {
	if text == "" {
		return "(absent)"
	}
	if len(text) > 200 {
		return text[:200] + "..."
	}
	return text
}

func ignoredPrefix(path string, ignored map[string]bool) bool {
	for prefix := range ignored {
		if strings.HasPrefix(path, prefix+".") {
			return true
		}
	}
	return false
}

// The network modes and the settings that change what Docker reports of a
// container beside them, each recreated through Tidewaiter's own swap.
func TestARealRecreateOfEachNetworkMode(t *testing.T) {
	client := realDocker(t, "alpine:latest")
	ctx := t.Context()
	base := func(name string, host map[string]any) map[string]any {
		return map[string]any{
			"Image": "alpine:latest", "Cmd": []string{"sleep", "3600"}, "Labels": map[string]string{e2eLabel: "local"},
			"HostConfig": host,
		}
	}
	create := func(name string, body map[string]any) {
		t.Helper()
		if err := client.quick(ctx, http.MethodPost, "/containers/create", map[string][]string{"name": {name}}, body, nil); err != nil {
			t.Fatalf("creating %s: %v", name, err)
		}
		if err := client.Start(ctx, name); err != nil {
			t.Fatalf("starting %s: %v", name, err)
		}
	}
	other := e2eName(t, client, "netns")
	create(other, base(other, map[string]any{}))

	for _, c := range []struct {
		what string
		body func(name string) map[string]any
	}{
		{"default bridge", func(name string) map[string]any { return base(name, map[string]any{}) }},
		{"a hostname of its own", func(name string) map[string]any {
			body := base(name, map[string]any{})
			body["Hostname"], body["Domainname"] = "tw-host", "example.test"
			return body
		}},
		{"host network", func(name string) map[string]any { return base(name, map[string]any{"NetworkMode": "host"}) }},
		{"no network", func(name string) map[string]any { return base(name, map[string]any{"NetworkMode": "none"}) }},
		{"another container's network", func(name string) map[string]any {
			return base(name, map[string]any{"NetworkMode": "container:" + other})
		}},
		{"a legacy link", func(name string) map[string]any { return base(name, map[string]any{"Links": []string{other + ":db"}}) }},
		{"privileged", func(name string) map[string]any { return base(name, map[string]any{"Privileged": true}) }},
		// As the docker CLI sends `--security-opt systempaths=unconfined`.
		{"systempaths unconfined", func(name string) map[string]any {
			return base(name, map[string]any{"MaskedPaths": []string{}, "ReadonlyPaths": []string{}})
		}},
		{"host pid, ipc and uts", func(name string) map[string]any {
			return base(name, map[string]any{"PidMode": "host", "IpcMode": "host", "UTSMode": "host"})
		}},
	} {
		t.Run(c.what, func(t *testing.T) {
			name := e2eName(t, client, "mode")
			create(name, c.body(name))
			recreated(t, client, name)
		})
	}
}
