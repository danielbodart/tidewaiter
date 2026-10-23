package docker

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/danielbodart/tidewaiter/internal/model"
)

// payload is a create payload as Docker receives it: JSON, read back
// generically, so the tests see exactly what goes on the wire.
func payload(t *testing.T, spec model.ContainerSpec) map[string]any {
	t.Helper()
	data, err := json.Marshal(CreatePayload(spec))
	if err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	if err := json.Unmarshal(data, &sent); err != nil {
		t.Fatal(err)
	}
	return sent
}

func jsonOf(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func expectJSON(t *testing.T, what string, got any, want string) {
	t.Helper()
	if jsonOf(got) != want {
		t.Errorf("%s: got %s, want %s", what, jsonOf(got), want)
	}
}

func TestCreateCarriesEnvLabelsCmdAndEntrypointVerbatim(t *testing.T) {
	sent := payload(t, fixture(t, "inspect-nginx.json").Spec())
	expectJSON(t, "Env", sent["Env"], `["PATH=/usr/local/sbin:/usr/local/bin","NGINX_VERSION=1.27"]`)
	expectJSON(t, "Cmd", sent["Cmd"], `["nginx","-g","daemon off;"]`)
	expectJSON(t, "Entrypoint", sent["Entrypoint"], `["/docker-entrypoint.sh"]`)
	expectJSON(t, "Labels", sent["Labels"], `{"com.docker.compose.project":"site","tidewaiter.autoupdate":"registry"}`)
	expectJSON(t, "ExposedPorts", sent["ExposedPorts"], `{"80/tcp":{}}`)
}

func TestCreateLeavesOutACmdTheContainerNeverSet(t *testing.T) {
	sent := payload(t, fixture(t, "inspect-multinet.json").Spec())
	if _, ok := sent["Cmd"]; ok {
		t.Errorf("sent Cmd %v, which would override the image's", sent["Cmd"])
	}
	// An empty one is a deliberate override, and is kept.
	emptied := spec("web")
	emptied.Entrypoint = []string{}
	expectJSON(t, "Entrypoint", payload(t, emptied)["Entrypoint"], `[]`)
}

func TestCreatePutsBindMountsInBindsWithTheReadOnlyFlag(t *testing.T) {
	host := payload(t, fixture(t, "inspect-nginx.json").Spec())["HostConfig"].(map[string]any)
	expectJSON(t, "Binds", host["Binds"], `["/srv/site:/usr/share/nginx/html:ro"]`)
}

func TestCreateReproducesPortBindingsByContainerPortAndProtocol(t *testing.T) {
	host := payload(t, fixture(t, "inspect-nginx.json").Spec())["HostConfig"].(map[string]any)
	expectJSON(t, "PortBindings", host["PortBindings"], `{"80/tcp":[{"HostIp":"0.0.0.0","HostPort":"8080"},{"HostIp":"::","HostPort":"8080"}]}`)
}

// A database published on loopback only must not come back on every
// interface after an update. Docker shows a plain `-p` with an empty HostIp,
// which stays unsent.
func TestCreateKeepsTheHostAddressOfEveryBinding(t *testing.T) {
	var raw Inspected
	if err := json.Unmarshal([]byte(`{"Name":"/db","Config":{"Image":"postgres:17"},"HostConfig":{"PortBindings":{
		"5432/tcp":[{"HostIp":"127.0.0.1","HostPort":"5432"},{"HostIp":"::1","HostPort":"5432"}],
		"9187/tcp":[{"HostIp":"","HostPort":"9187"}]}}}`), &raw); err != nil {
		t.Fatal(err)
	}
	spec := raw.Spec()
	host := payload(t, spec)["HostConfig"].(map[string]any)
	expectJSON(t, "PortBindings", host["PortBindings"],
		`{"5432/tcp":[{"HostIp":"127.0.0.1","HostPort":"5432"},{"HostIp":"::1","HostPort":"5432"}],"9187/tcp":[{"HostPort":"9187"}]}`)
	// Activity and health still see each port once.
	if want := []model.PublishedPort{port(5432, 5432, model.TCP), port(9187, 9187, model.TCP)}; !reflect.DeepEqual(spec.Published(), want) {
		t.Errorf("published %v", spec.Published())
	}
}

func TestCreateCarriesRestartPolicyStopSignalAndStopTimeout(t *testing.T) {
	sent := payload(t, fixture(t, "inspect-nginx.json").Spec())
	expectJSON(t, "StopSignal", sent["StopSignal"], `"SIGQUIT"`)
	expectJSON(t, "StopTimeout", sent["StopTimeout"], `15`)
	expectJSON(t, "RestartPolicy", sent["HostConfig"].(map[string]any)["RestartPolicy"], `{"MaximumRetryCount":0,"Name":"unless-stopped"}`)
}

func TestCreateCarriesTheHealthcheck(t *testing.T) {
	sent := payload(t, fixture(t, "inspect-nginx.json").Spec())
	expectJSON(t, "Healthcheck", sent["Healthcheck"], `{"Interval":30000000000,"Retries":3,"Test":["CMD-SHELL","curl -f http://localhost/ || exit 1"],"Timeout":3000000000}`)
}

func TestCreatePutsNamedVolumesInMountsLongFormNotBinds(t *testing.T) {
	host := payload(t, fixture(t, "inspect-multinet.json").Spec())["HostConfig"].(map[string]any)
	expectJSON(t, "Mounts", host["Mounts"], `[{"ReadOnly":false,"Source":"api-data","Target":"/data","Type":"volume"}]`)
	expectJSON(t, "Binds", host["Binds"], `[]`)
}

func TestCreateCarriesCapsSysctlsExtraHostsUserAndWorkingDir(t *testing.T) {
	sent := payload(t, fixture(t, "inspect-multinet.json").Spec())
	host := sent["HostConfig"].(map[string]any)
	expectJSON(t, "CapAdd", host["CapAdd"], `["NET_ADMIN"]`)
	expectJSON(t, "CapDrop", host["CapDrop"], `["MKNOD"]`)
	expectJSON(t, "Sysctls", host["Sysctls"], `{"net.core.somaxconn":"1024"}`)
	expectJSON(t, "ExtraHosts", host["ExtraHosts"], `["db:10.0.0.9"]`)
	expectJSON(t, "NetworkMode", host["NetworkMode"], `"frontend"`)
	expectJSON(t, "User", sent["User"], `"app"`)
	expectJSON(t, "WorkingDir", sent["WorkingDir"], `"/app"`)
}

func TestCreateCarriesDevicesAndPrivileged(t *testing.T) {
	privileged := spec("web")
	privileged.Privileged = true
	privileged.Devices = []model.Device{{PathOnHost: "/dev/dri", PathInContainer: "/dev/dri", CgroupPermissions: "rwm"}}
	host := payload(t, privileged)["HostConfig"].(map[string]any)
	expectJSON(t, "Privileged", host["Privileged"], `true`)
	expectJSON(t, "Devices", host["Devices"], `[{"CgroupPermissions":"rwm","PathInContainer":"/dev/dri","PathOnHost":"/dev/dri"}]`)
}

func TestCreateAttachesOnlyTheFirstNetworkWithItsAliasesAndAddress(t *testing.T) {
	sent := payload(t, fixture(t, "inspect-multinet.json").Spec())
	expectJSON(t, "NetworkingConfig", sent["NetworkingConfig"], `{"EndpointsConfig":{"frontend":{"Aliases":["api"]}}}`)
	expectJSON(t, "backend", endpointOf(ExtraNetworks(fixture(t, "inspect-multinet.json").Spec())[0]), `{"Aliases":["api-internal"],"IPAMConfig":{"IPv4Address":"172.28.0.5"}}`)
}

func TestCreateNeverLeaksARuntimeOnlyField(t *testing.T) {
	sent := payload(t, fixture(t, "inspect-multinet.json").Spec())
	for _, field := range []string{"Id", "State", "NetworkSettings", "Mounts", "Name"} {
		if _, ok := sent[field]; ok {
			t.Errorf("sent %s", field)
		}
	}
	// The runtime IP is live state; only a static pin is ever asked for.
	if strings.Contains(jsonOf(sent), "172.27.0.9") {
		t.Errorf("sent the runtime IP: %s", jsonOf(sent))
	}
}

// What a create asks for is, read back as Docker shows it, the container
// that was there: the faithful recreate, field for field.
func TestARecreatedContainerInspectsAsTheOriginal(t *testing.T) {
	for _, name := range []string{"inspect-nginx.json", "inspect-multinet.json"} {
		original := fixture(t, name).Spec()
		recreated, err := specOf(original.Name, []byte(jsonOf(CreatePayload(original))))
		if err != nil {
			t.Fatal(err)
		}
		// Runtime IPs are not recreated, and later networks are connected after.
		want := original
		want.Networks = []model.NetworkAttachment{original.Networks[0]}
		want.Networks[0].IPAddress = ""
		if !reflect.DeepEqual(recreated, want) {
			t.Errorf("%s:\ngot  %+v\nwant %+v", name, recreated, want)
		}
	}
}

func TestExtraNetworksIsEveryNetworkBeyondTheFirst(t *testing.T) {
	if extra := ExtraNetworks(fixture(t, "inspect-multinet.json").Spec()); len(extra) != 1 || extra[0].Name != "backend" {
		t.Errorf("got %+v", extra)
	}
	if extra := ExtraNetworks(fixture(t, "inspect-nginx.json").Spec()); len(extra) != 0 {
		t.Errorf("got %+v", extra)
	}
}

func TestRefusesAContainerWithAnAnonymousVolumeRatherThanLoseItsData(t *testing.T) {
	if reason := Unrecreatable(fixture(t, "inspect-anonvol.json").Spec()); !strings.Contains(reason, "anonymous volume") {
		t.Errorf("got %q", reason)
	}
	for _, name := range []string{"inspect-nginx.json", "inspect-multinet.json"} {
		if reason := Unrecreatable(fixture(t, name).Spec()); reason != "" {
			t.Errorf("%s: got %q", name, reason)
		}
	}
}

func TestPlanSwap(t *testing.T) {
	base := spec("web")
	base.Networks = []model.NetworkAttachment{{Name: "frontend"}, {Name: "backend"}}
	newSpec := base
	newSpec.Image = "web:new"
	plan := PlanSwap(base, newSpec, 12)

	t.Run("stops, parks the old, creates and starts the new under the original name", func(t *testing.T) {
		want := []Step{
			{Op: Stop, Container: "web", Grace: 12},
			{Op: Rename, Container: "web", To: RollbackName("web")},
			{Op: Create, Spec: newSpec},
			{Op: Connect, Container: "web", Network: model.NetworkAttachment{Name: "backend"}},
			{Op: Start, Container: "web"},
		}
		if !reflect.DeepEqual(plan.Swap, want) {
			t.Errorf("got %+v", plan.Swap)
		}
	})
	t.Run("commit removes the parked old container", func(t *testing.T) {
		if want := []Step{{Op: Remove, Container: RollbackName("web"), Force: true}}; !reflect.DeepEqual(plan.Commit, want) {
			t.Errorf("got %+v", plan.Commit)
		}
	})
	t.Run("rollback removes the new one and restores the old name", func(t *testing.T) {
		want := []Step{
			{Op: Remove, Container: "web", Force: true},
			{Op: Rename, Container: RollbackName("web"), To: "web"},
			{Op: Start, Container: "web"},
		}
		if !reflect.DeepEqual(plan.Rollback, want) {
			t.Errorf("got %+v", plan.Rollback)
		}
	})
}

func TestRepository(t *testing.T) {
	for ref, want := range map[string]string{
		"app:latest":                 "app",
		"nginx":                      "nginx",
		"ghcr.io/me/app:1":           "ghcr.io/me/app",
		"localhost:5000/app":         "localhost:5000/app",
		"localhost:5000/app:2":       "localhost:5000/app",
		"app@sha256:abc":             "app",
		"ghcr.io/me/app:1@sha256:ab": "ghcr.io/me/app",
	} {
		if got := repository(ref); got != want {
			t.Errorf("repository(%q) = %q, want %q", ref, got, want)
		}
	}
}
