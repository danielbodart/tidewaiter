package docker

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/danielbodart/tidewaiter/internal/decide"
	"github.com/danielbodart/tidewaiter/internal/model"
)

// raw is value as Docker would write it.
func raw(value any) json.RawMessage {
	data, _ := json.Marshal(value)
	return data
}

// attached is a container on two networks, front its own, as inspect in
// version shows it, with change made to that inspect.
func attached(t *testing.T, version string, change func(*model.Inspect)) model.ContainerSpec {
	t.Helper()
	spec := spec("web")
	spec.Networks = []model.NetworkAttachment{{Name: "front", Aliases: []string{"web"}}, {Name: "back", Aliases: []string{}}}
	spec.NetworkMode = "front"
	spec = inspected(t, spec, version)
	change(spec.Inspected)
	return spec
}

func endpointSent(t *testing.T, body map[string]any, network string) map[string]any {
	t.Helper()
	var sent struct {
		NetworkingConfig struct{ EndpointsConfig map[string]map[string]any }
	}
	json.Unmarshal([]byte(jsonOf(body)), &sent)
	return sent.NetworkingConfig.EndpointsConfig[network]
}

func createdAt(t *testing.T, spec model.ContainerSpec, version string) map[string]any {
	t.Helper()
	body, err := CreateBody(spec, version)
	if err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	json.Unmarshal([]byte(jsonOf(body)), &sent)
	return sent
}

// What Tidewaiter has never heard of goes back as Docker reported it: in
// Config, HostConfig and a network's endpoint settings alike.
func TestCreatePassesThroughWhatItDoesNotKnow(t *testing.T) {
	spec := attached(t, NewestAPI, func(inspect *model.Inspect) {
		inspect.Config["Domainname"] = raw("example.test")
		inspect.Config["SomethingNew"] = raw(map[string]any{"deep": []int{1, 2}})
		inspect.HostConfig["Memory"] = raw(1 << 26)
		inspect.HostConfig["Ulimits"] = raw([]map[string]any{{"Name": "nofile", "Soft": 1024, "Hard": 2048}})
		inspect.HostConfig["LogConfig"] = raw(map[string]any{"Type": "json-file", "Config": map[string]string{"max-size": "1m"}})
		inspect.Endpoints["front"]["DriverOpts"] = raw(map[string]string{"com.example": "1"})
		inspect.Endpoints["front"]["SomethingNew"] = raw(true)
	})
	sent := createdAt(t, spec, NewestAPI)
	expectJSON(t, "Domainname", sent["Domainname"], `"example.test"`)
	expectJSON(t, "SomethingNew", sent["SomethingNew"], `{"deep":[1,2]}`)
	host := sent["HostConfig"].(map[string]any)
	expectJSON(t, "Memory", host["Memory"], `67108864`)
	expectJSON(t, "Ulimits", host["Ulimits"], `[{"Hard":2048,"Name":"nofile","Soft":1024}]`)
	expectJSON(t, "LogConfig", host["LogConfig"], `{"Config":{"max-size":"1m"},"Type":"json-file"}`)
	front := endpointSent(t, sent, "front")
	expectJSON(t, "DriverOpts", front["DriverOpts"], `{"com.example":"1"}`)
	expectJSON(t, "endpoint SomethingNew", front["SomethingNew"], `true`)
}

// And it survives a swap, and the swap after, in the fake as in Docker.
func TestWhatItDoesNotKnowSurvivesOneSwapAfterAnother(t *testing.T) {
	w := newWorld(t)
	w.opted("web", "sha256:old", "sha256:new", labels(dockerHealth, map[string]string{IdleSamplesLabel: "1"}))
	w.docker.with(func(f *fakeDocker) {
		f.extras[f.running[0].ID] = inspectExtra{HostConfig: map[string]any{"Memory": 1 << 26, "Init": true}}
	})
	w.expect(t, decide.Update)
	w.registry.set("app:latest", "sha256:newer")
	w.docker.with(func(f *fakeDocker) {
		f.pullLands["app:latest"] = "sha256:newer"
		f.healthByName["web"] = model.Healthy
	})
	w.expect(t, decide.Update)

	payloads := read(w.docker, func(f *fakeDocker) []map[string]any { return f.payloads })
	if len(payloads) != 2 {
		t.Fatalf("created %d times", len(payloads))
	}
	for _, sent := range payloads {
		host := sent["HostConfig"].(map[string]any)
		expectJSON(t, "Memory", host["Memory"], `67108864`)
		expectJSON(t, "Init", host["Init"], `true`)
	}
}

// A hostname that is Docker's default, the old container's short id, is
// left for Docker to give the new container its own; one the container was
// given is kept.
func TestCreateLeavesADefaultHostnameToDocker(t *testing.T) {
	defaulted := attached(t, NewestAPI, func(*model.Inspect) {})
	if hostname, ok := createdAt(t, defaulted, NewestAPI)["Hostname"]; ok {
		t.Errorf("sent the old id as the hostname: %v", hostname)
	}
	own := attached(t, NewestAPI, func(inspect *model.Inspect) { inspect.Config["Hostname"] = raw("tw-host") })
	expectJSON(t, "Hostname", createdAt(t, own, NewestAPI)["Hostname"], `"tw-host"`)
}

// Sharing another container's network, a container shows that one's
// hostname, and Docker refuses a create that gives any.
func TestCreateLeavesOutTheHostnameInContainerNetworkMode(t *testing.T) {
	spec := attached(t, NewestAPI, func(inspect *model.Inspect) {
		inspect.Config["Hostname"] = raw("other-host")
		inspect.HostConfig["NetworkMode"] = raw("container:0123456789ab")
	})
	if hostname, ok := createdAt(t, spec, NewestAPI)["Hostname"]; ok {
		t.Errorf("sent %v", hostname)
	}
}

// Docker's default masked and read-only paths are left for Docker to fill
// in again; none at all is systempaths=unconfined, and kept.
func TestCreateLeavesDockersDefaultSystemPathsToDocker(t *testing.T) {
	defaulted := attached(t, NewestAPI, func(*model.Inspect) {})
	host := createdAt(t, defaulted, NewestAPI)["HostConfig"].(map[string]any)
	for _, key := range []string{"MaskedPaths", "ReadonlyPaths"} {
		if paths, ok := host[key]; ok {
			t.Errorf("sent %s %v", key, paths)
		}
	}
	unconfined := attached(t, NewestAPI, func(inspect *model.Inspect) {
		inspect.HostConfig["MaskedPaths"], inspect.HostConfig["ReadonlyPaths"] = raw([]string{}), raw([]string{})
	})
	host = createdAt(t, unconfined, NewestAPI)["HostConfig"].(map[string]any)
	expectJSON(t, "MaskedPaths", host["MaskedPaths"], `[]`)
	expectJSON(t, "ReadonlyPaths", host["ReadonlyPaths"], `[]`)
}

// An endpoint's operational data is where the container is attached now,
// and is not sent; its configuration is.
func TestCreateSendsAnEndpointsConfigurationNotItsOperationalData(t *testing.T) {
	spec := attached(t, NewestAPI, func(inspect *model.Inspect) {
		inspect.Endpoints["front"]["IPAMConfig"] = raw(map[string]string{"IPv4Address": "10.0.0.5"})
		inspect.Endpoints["front"]["Gateway"] = raw("10.0.0.1")
		inspect.Endpoints["front"]["GwPriority"] = raw(5)
	})
	front := endpointSent(t, createdAt(t, spec, NewestAPI), "front")
	for _, key := range operational {
		if key == "MacAddress" {
			continue
		}
		if value, ok := front[key]; ok {
			t.Errorf("sent %s %v", key, value)
		}
	}
	expectJSON(t, "IPAMConfig", front["IPAMConfig"], `{"IPv4Address":"10.0.0.5"}`)
	expectJSON(t, "GwPriority", front["GwPriority"], `5`)
	expectJSON(t, "Aliases", front["Aliases"], `["web"]`)
}

// Before 1.45 inspect adds the short id to a user-defined network's
// aliases; it is not the container's own, and is not sent.
func TestCreateLeavesOutTheShortIDInspectAddsToAliasesBefore145(t *testing.T) {
	spec := attached(t, "1.44", func(*model.Inspect) {})
	if aliases := jsonOf(spec.Inspected.Endpoints["front"]["Aliases"]); !strings.Contains(aliases, "0123456789ab") {
		t.Fatalf("the fake did not add it: %s", aliases)
	}
	expectJSON(t, "Aliases", endpointSent(t, createdAt(t, spec, "1.44"), "front")["Aliases"], `["web"]`)
}

// Where a MAC address goes moved at 1.44, from the container's Config to
// its network's endpoint, and 1.52 stopped taking it in Config; inspect
// shows the one asked for in Config until 1.52, and from then only on each
// network. A network connected after the create keeps its own from 1.54,
// the first connect that applies it, and only once inspect says which is
// its own, from 1.52.
func TestAMacAddressGoesWhereItsVersionTakesIt(t *testing.T) {
	const asked, picked, other = "02:00:00:aa:bb:cc", "02:42:ac:11:00:02", "02:00:00:dd:ee:ff"
	for _, c := range []struct {
		version    string
		inConfig   string
		onEndpoint string
		connected  string
	}{
		{"1.43", asked, "", ""},
		{"1.44", "", asked, ""},
		{"1.51", "", asked, ""},
		{"1.52", "", asked, ""},
		{"1.53", "", asked, ""},
		{"1.54", "", asked, other},
	} {
		spec := attached(t, c.version, func(inspect *model.Inspect) {
			if atLeast(c.version, apiInspectEndpointMAC) {
				inspect.Endpoints["front"]["MacAddress"] = raw(asked)
			} else {
				inspect.Config["MacAddress"] = raw(asked)
				inspect.Endpoints["front"]["MacAddress"] = raw(picked)
			}
			inspect.Endpoints["back"]["MacAddress"] = raw(other)
		})
		sent := createdAt(t, spec, c.version)
		inConfig, _ := sent["MacAddress"].(string)
		onEndpoint, _ := endpointSent(t, sent, "front")["MacAddress"].(string)
		connected, _ := ConnectBody(spec, "web", "back", c.version)["EndpointConfig"].(map[string]any)["MacAddress"].(string)
		if inConfig != c.inConfig || onEndpoint != c.onEndpoint || connected != c.connected {
			t.Errorf("v%s: in Config %q, on the endpoint %q, connected %q", c.version, inConfig, onEndpoint, connected)
		}
	}
}

// A healthcheck keeps what the container set and anything Tidewaiter does
// not know of, and leaves the fields the old image gave it to the new one.
func TestCreateKeepsTheContainersOwnHealthcheckFields(t *testing.T) {
	s := spec("web")
	s.Healthcheck = &model.Healthcheck{Test: []string{"CMD", "/check"}, Interval: 5 * time.Second, Retries: 3, StartInterval: time.Second}
	s = inspected(t, s, NewestAPI)
	var check map[string]json.RawMessage
	json.Unmarshal(s.Inspected.Config["Healthcheck"], &check)
	check["SomethingNew"] = raw(1)
	s.Inspected.Config["Healthcheck"] = raw(check)
	own := WithoutImageDefaults(s, ImageDefaults{Healthcheck: &model.Healthcheck{Test: []string{"CMD", "/check"}, Retries: 3}})
	expectJSON(t, "Healthcheck", createdAt(t, own, NewestAPI)["Healthcheck"], `{"Interval":5000000000,"SomethingNew":1,"StartInterval":1000000000}`)
}

// An image's VOLUMEs are merged into a container's config like its other
// defaults, and left to the new image the same way, path by path.
func TestCreateLeavesTheOldImagesVolumesToTheNewImage(t *testing.T) {
	s := spec("web")
	s.Volumes = []string{"/data", "/var/lib/app"}
	own := WithoutImageDefaults(inspected(t, s, NewestAPI), ImageDefaults{Volumes: []string{"/var/lib/app"}})
	expectJSON(t, "Volumes", createdAt(t, own, NewestAPI)["Volumes"], `{"/data":{}}`)
	all := WithoutImageDefaults(inspected(t, s, NewestAPI), ImageDefaults{Volumes: s.Volumes})
	if volumes, ok := createdAt(t, all, NewestAPI)["Volumes"]; ok {
		t.Errorf("sent %v", volumes)
	}
}

// A spec no inspect read has nothing to pass through, and is refused rather
// than recreated from the few fields Tidewaiter reads.
func TestCreateRefusesASpecWithNoInspect(t *testing.T) {
	if _, err := CreateBody(spec("web"), NewestAPI); err == nil {
		t.Error("created from nothing")
	}
}
