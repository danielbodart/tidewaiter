package docker

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

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

// With an image that sets nothing, everything the container has is its own,
// and goes on the wire as it is.
func TestCreateCarriesTheContainersOwnEnvLabelsCmdAndEntrypoint(t *testing.T) {
	sent := payload(t, WithoutImageDefaults(fixture(t, "inspect-nginx.json").Spec(), ImageDefaults{}))
	expectJSON(t, "Env", sent["Env"], `["PATH=/usr/local/sbin:/usr/local/bin","NGINX_VERSION=1.27"]`)
	expectJSON(t, "Cmd", sent["Cmd"], `["nginx","-g","daemon off;"]`)
	expectJSON(t, "Entrypoint", sent["Entrypoint"], `["/docker-entrypoint.sh"]`)
	expectJSON(t, "Labels", sent["Labels"], `{"com.docker.compose.project":"site","tidewaiter.autoupdate":"registry"}`)
	expectJSON(t, "ExposedPorts", sent["ExposedPorts"], `{"80/tcp":{}}`)
	expectJSON(t, "StopSignal", sent["StopSignal"], `"SIGQUIT"`)
}

// nginx's own image, as the fixture's container inherited it.
var nginxImage = ImageDefaults{
	Env:         []string{"PATH=/usr/local/sbin:/usr/local/bin", "NGINX_VERSION=1.27"},
	Cmd:         []string{"nginx", "-g", "daemon off;"},
	Entrypoint:  []string{"/docker-entrypoint.sh"},
	Labels:      map[string]string{"maintainer": "NGINX Docker Maintainers"},
	Exposed:     []string{"80/tcp"},
	StopSignal:  "SIGQUIT",
	Healthcheck: &model.Healthcheck{Test: []string{"CMD-SHELL", "curl -f http://localhost/ || exit 1"}, Interval: 30 * time.Second, Timeout: 3 * time.Second, Retries: 3},
}

// Everything that equals the old image's default is left out, so Docker
// fills it from the new image; what the container set itself stays.
func TestCreateLeavesWhatTheOldImageSetToTheNewImage(t *testing.T) {
	sent := payload(t, WithoutImageDefaults(fixture(t, "inspect-nginx.json").Spec(), nginxImage))
	for _, field := range []string{"Cmd", "Entrypoint", "StopSignal", "Healthcheck", "User", "WorkingDir"} {
		if value, ok := sent[field]; ok {
			t.Errorf("sent %s %s, which would pin the old image's", field, jsonOf(value))
		}
	}
	expectJSON(t, "Env", sent["Env"], `[]`)
	expectJSON(t, "Labels", sent["Labels"], `{"com.docker.compose.project":"site","tidewaiter.autoupdate":"registry"}`)
	// Exposed by the image, but published by the container: kept, so the
	// binding has its port even on an image that stops exposing it.
	expectJSON(t, "ExposedPorts", sent["ExposedPorts"], `{"80/tcp":{}}`)
	expectJSON(t, "StopTimeout", sent["StopTimeout"], `15`)
}

func TestEnvAndLabelsDropOnlyTheEntriesEqualToTheImages(t *testing.T) {
	spec := spec("web")
	spec.Env = []string{"PATH=/usr/bin", "MODE=dev", "TZ=UTC", "FLAG"}
	spec.Labels = map[string]string{"version": "1", "vendor": "me", AutoupdateLabel: "registry"}
	own := WithoutImageDefaults(spec, ImageDefaults{Env: []string{"PATH=/usr/bin", "MODE=prod", "FLAG"}, Labels: map[string]string{"version": "1", "vendor": "acme"}})
	expectJSON(t, "Env", own.Env, `["MODE=dev","TZ=UTC"]`)
	expectJSON(t, "Labels", own.Labels, `{"tidewaiter.autoupdate":"registry","vendor":"me"}`)
	// The spec read off inspect is left as it was: policy is read from it.
	expectJSON(t, "the original's labels", spec.Labels, `{"tidewaiter.autoupdate":"registry","vendor":"me","version":"1"}`)
}

func TestAnExposedPortTheImageDeclaresIsDroppedUnlessABindingPublishesIt(t *testing.T) {
	spec := spec("web")
	spec.Exposed = []string{"80/tcp", "443/tcp", "9000/tcp", "53/udp"}
	spec.Bindings = []model.PortBinding{{PublishedPort: port(0, 443, model.TCP)}}
	own := WithoutImageDefaults(spec, ImageDefaults{Exposed: []string{"80/tcp", "443/tcp", "53/udp"}})
	expectJSON(t, "Exposed", own.Exposed, `["443/tcp","9000/tcp"]`)
}

func TestUserWorkingDirAndStopSignalAreKeptOnlyWhenTheContainerSetItsOwn(t *testing.T) {
	spec := spec("web")
	spec.User, spec.WorkingDir, spec.StopSignal = "app", "/srv", "SIGINT"
	same := WithoutImageDefaults(spec, ImageDefaults{User: "app", WorkingDir: "/srv", StopSignal: "SIGINT"})
	if same.User != "" || same.WorkingDir != "" || same.StopSignal != "" {
		t.Errorf("kept %+v", same)
	}
	other := WithoutImageDefaults(spec, ImageDefaults{User: "root", WorkingDir: "/", StopSignal: "SIGTERM"})
	if other.User != "app" || other.WorkingDir != "/srv" || other.StopSignal != "SIGINT" {
		t.Errorf("dropped %+v", other)
	}
}

// A Cmd the user typed that is the image's own cannot be told from the
// image's: it is left to the new image, as a fresh `docker run` without it
// would be.
func TestACmdEqualToTheImagesIsLeftToTheNewImage(t *testing.T) {
	spec := spec("web")
	spec.Entrypoint, spec.Cmd = []string{"/entry"}, []string{"run"}
	own := WithoutImageDefaults(spec, ImageDefaults{Entrypoint: []string{"/entry"}, Cmd: []string{"run"}})
	if own.Entrypoint != nil || own.Cmd != nil {
		t.Errorf("kept %v %v", own.Entrypoint, own.Cmd)
	}
	// Arguments of the container's own go to the new image's entrypoint.
	spec.Cmd = []string{"serve", "--fast"}
	own = WithoutImageDefaults(spec, ImageDefaults{Entrypoint: []string{"/entry"}, Cmd: []string{"run"}})
	if own.Entrypoint != nil || !slices.Equal(own.Cmd, []string{"serve", "--fast"}) {
		t.Errorf("got %v %v", own.Entrypoint, own.Cmd)
	}
}

// Docker never fills Cmd beside an Entrypoint of the container's own, so
// that Cmd is the user's even when it is the image's - dropped, the new
// image's CMD would reach the override as arguments it was never given.
func TestAnEntrypointOverrideKeepsItsCmdEvenWhenItMatchesTheImages(t *testing.T) {
	spec := spec("web")
	spec.Entrypoint, spec.Cmd = []string{"/bin/other"}, []string{"run"}
	sent := payload(t, WithoutImageDefaults(spec, ImageDefaults{Entrypoint: []string{"/entry"}, Cmd: []string{"run"}}))
	expectJSON(t, "Entrypoint", sent["Entrypoint"], `["/bin/other"]`)
	expectJSON(t, "Cmd", sent["Cmd"], `["run"]`)
}

// `--entrypoint /entry` on an image whose ENTRYPOINT is /entry: Docker
// stores no Cmd, which is what gives the override away. Kept, and still
// sent with no Cmd, so the new image's CMD is not handed to it.
func TestAnEntrypointOverrideEqualToTheImagesIsKeptWhenItLeftCmdEmpty(t *testing.T) {
	image := ImageDefaults{Entrypoint: []string{"/entry"}, Cmd: []string{"run"}}
	for _, cmd := range [][]string{nil, {}} {
		spec := spec("web")
		spec.Entrypoint, spec.Cmd = []string{"/entry"}, cmd
		sent := payload(t, WithoutImageDefaults(spec, image))
		expectJSON(t, "Entrypoint", sent["Entrypoint"], `["/entry"]`)
		if got, ok := sent["Cmd"]; ok && jsonOf(got) != `[]` {
			t.Errorf("sent Cmd %s", jsonOf(got))
		}
	}
}

// An Entrypoint of [] is "none", not "the image's": kept, even on an image
// that has none either. Docker then fills Cmd from the image, so a Cmd equal
// to the old image's is its, and goes.
func TestAnEmptyEntrypointIsKeptAsAnOverrideButNilIsLeftToTheImage(t *testing.T) {
	spec := spec("web")
	spec.Entrypoint, spec.Cmd = []string{}, []string{"run"}
	for _, imageEntrypoint := range [][]string{{"/entry"}, nil} {
		own := WithoutImageDefaults(spec, ImageDefaults{Entrypoint: imageEntrypoint, Cmd: []string{"run"}})
		expectJSON(t, "Entrypoint", payload(t, own)["Entrypoint"], `[]`)
		if own.Cmd != nil {
			t.Errorf("kept the image's Cmd %v", own.Cmd)
		}
	}
	spec.Entrypoint = nil
	if own := WithoutImageDefaults(spec, ImageDefaults{Cmd: []string{"run"}}); own.Entrypoint != nil {
		t.Errorf("got %v", own.Entrypoint)
	}
}

// Docker fills a healthcheck field by field, so an override of one keeps
// that one alone and the new image's test still takes effect.
func TestAHealthcheckOverrideKeepsOnlyTheFieldsTheContainerSet(t *testing.T) {
	image := &model.Healthcheck{Test: []string{"CMD", "/v1/check"}, Interval: 30 * time.Second, Retries: 3}
	spec := spec("web")
	spec.Healthcheck = &model.Healthcheck{Test: []string{"CMD", "/v1/check"}, Interval: 5 * time.Second, Retries: 3}
	sent := payload(t, WithoutImageDefaults(spec, ImageDefaults{Healthcheck: image}))
	expectJSON(t, "Healthcheck", sent["Healthcheck"], `{"Interval":5000000000}`)

	spec.Healthcheck = &model.Healthcheck{Test: []string{"NONE"}}
	expectJSON(t, "disabled", payload(t, WithoutImageDefaults(spec, ImageDefaults{Healthcheck: image}))["Healthcheck"], `{"Test":["NONE"]}`)
	// The container's own, on an image with none, is kept whole.
	spec.Healthcheck = image
	if own := WithoutImageDefaults(spec, ImageDefaults{}); !reflect.DeepEqual(own.Healthcheck, image) {
		t.Errorf("got %+v", own.Healthcheck)
	}
}

// The property the recreate exists for: a container recreated onto a new
// image is what creating it afresh, from the same settings of its own, on
// that image gives - the new image's defaults where it set none, its own
// everywhere else. Each case is one way a user can have set things.
func TestARecreateOnANewImageIsAFreshCreateOfTheContainersOwnSettings(t *testing.T) {
	v1 := ImageDefaults{
		Env: []string{"PATH=/v1/bin", "APP_VERSION=1", "APP_MODE=prod"}, Cmd: []string{"serve", "--v1"}, Entrypoint: []string{"/entry"},
		Labels: map[string]string{"version": "1", "vendor": "acme"}, Exposed: []string{"8080/tcp", "9000/tcp"},
		User: "app", WorkingDir: "/v1", StopSignal: "SIGQUIT",
		Healthcheck: &model.Healthcheck{Test: []string{"CMD", "/v1/check"}, Interval: 30 * time.Second},
	}
	v2 := ImageDefaults{
		Env: []string{"PATH=/v2/bin", "APP_VERSION=2", "APP_MODE=prod"}, Cmd: []string{"serve", "--v2"}, Entrypoint: []string{"/entry2"},
		Labels: map[string]string{"version": "2", "vendor": "acme"}, Exposed: []string{"8080/tcp"},
		User: "app", WorkingDir: "/v2", StopSignal: "SIGTERM",
		Healthcheck: &model.Healthcheck{Test: []string{"CMD", "/v2/check"}, Interval: 30 * time.Second},
	}
	for name, change := range map[string]func(*model.ContainerSpec){
		"nothing of its own": func(*model.ContainerSpec) {},
		"its own env, labels, user and dir": func(s *model.ContainerSpec) {
			s.Env = []string{"APP_MODE=dev", "TZ=UTC"}
			s.Labels["team"] = "web"
			s.User, s.WorkingDir, s.StopSignal = "nobody", "/data", "SIGINT"
		},
		"arguments of its own":          func(s *model.ContainerSpec) { s.Cmd = []string{"migrate"} },
		"an entrypoint of its own":      func(s *model.ContainerSpec) { s.Entrypoint = []string{"/bin/sh", "-c"} },
		"an entrypoint and arguments":   func(s *model.ContainerSpec) { s.Entrypoint, s.Cmd = []string{"/bin/sh"}, []string{"-c", "true"} },
		"the image's entrypoint, named": func(s *model.ContainerSpec) { s.Entrypoint = []string{"/entry"} },
		"an entrypoint with the image's arguments": func(s *model.ContainerSpec) {
			s.Entrypoint, s.Cmd = []string{"/bin/sh"}, []string{"serve", "--v1"}
		},
		"no entrypoint at all":   func(s *model.ContainerSpec) { s.Entrypoint = []string{} },
		"a healthcheck interval": func(s *model.ContainerSpec) { s.Healthcheck = &model.Healthcheck{Interval: 5 * time.Second} },
		"a published port": func(s *model.ContainerSpec) {
			s.Bindings = bound(port(9000, 9000, model.TCP))
			s.Exposed = []string{"9000/tcp"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			own := spec("web")
			change(&own)
			running := merged(own, v1)
			recreated, err := specOf("web", []byte(jsonOf(CreatePayload(WithoutImageDefaults(running, v1)))))
			if err != nil {
				t.Fatal(err)
			}
			if got, want := merged(recreated, v2), merged(own, v2); !reflect.DeepEqual(got, want) {
				t.Errorf("recreated as\n%+v\nwant\n%+v", got, want)
			}
		})
	}
}

func TestCreateLeavesOutACmdTheContainerNeverSet(t *testing.T) {
	sent := payload(t, fixture(t, "inspect-multinet.json").Spec())
	if _, ok := sent["Cmd"]; ok {
		t.Errorf("sent Cmd %v, which would override the image's", sent["Cmd"])
	}
	// An empty Entrypoint is a deliberate override, and is kept.
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

// `-p 80` and `-p 127.0.0.1::81` leave the host port to Docker, and
// `-p 8000-8010:82` has it pick from a range - as Docker stores them. Each is
// replayed so Docker picks again, address kept, rather than dropped, which
// would stop the recreated container publishing the port at all.
func TestCreateReplaysABindingWhoseHostPortDockerPicks(t *testing.T) {
	var raw Inspected
	if err := json.Unmarshal([]byte(`{"Name":"/web","Config":{"Image":"alpine"},"HostConfig":{"PortBindings":{
		"80/tcp":[{"HostIp":"","HostPort":""}],
		"81/tcp":[{"HostIp":"127.0.0.1","HostPort":""}],
		"82/tcp":[{"HostIp":"","HostPort":"18000-18010"}],
		"90/tcp":[{"HostIp":"","HostPort":"18100"}],
		"91/udp":[{"HostIp":"","HostPort":"18101"}]}}}`), &raw); err != nil {
		t.Fatal(err)
	}
	spec := raw.Spec()
	want := []model.PortBinding{
		{PublishedPort: port(0, 80, model.TCP)},
		{HostIP: "127.0.0.1", PublishedPort: port(0, 81, model.TCP)},
		{HostRange: "18000-18010", PublishedPort: port(0, 82, model.TCP)},
		{PublishedPort: port(18100, 90, model.TCP)},
		{PublishedPort: port(18101, 91, model.UDP)},
	}
	if !reflect.DeepEqual(spec.Bindings, want) {
		t.Errorf("read %+v", spec.Bindings)
	}
	host := payload(t, spec)["HostConfig"].(map[string]any)
	expectJSON(t, "PortBindings", host["PortBindings"],
		`{"80/tcp":[{}],"81/tcp":[{"HostIp":"127.0.0.1"}],"82/tcp":[{"HostPort":"18000-18010"}],"90/tcp":[{"HostPort":"18100"}],"91/udp":[{"HostPort":"18101"}]}`)
	// And read back as Docker would show it, the same bindings again.
	recreated, err := specOf("web", []byte(jsonOf(CreatePayload(spec))))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(recreated.Bindings, want) {
		t.Errorf("recreated %+v", recreated.Bindings)
	}
}

// Which host port Docker gave a picked binding is not in the spec, so
// activity and health are never handed a port 0 to watch or dial.
func TestPublishedLeavesOutABindingWhoseHostPortDockerPicks(t *testing.T) {
	spec := spec("web")
	spec.Bindings = []model.PortBinding{
		{PublishedPort: port(0, 80, model.TCP)},
		{HostRange: "18000-18010", PublishedPort: port(0, 82, model.TCP)},
		{PublishedPort: port(8443, 443, model.TCP)},
	}
	if want := []model.PublishedPort{port(8443, 443, model.TCP)}; !reflect.DeepEqual(spec.Published(), want) {
		t.Errorf("published %v", spec.Published())
	}
	// Nor does a tidewaiter.ports override find one to select.
	policy, warnings := ParseLabels(AutoupdateLabel, map[string]string{AutoupdateLabel: "registry", PortsLabel: "80"}, spec.Published())
	if len(policy.Ports) != 0 || len(warnings) != 1 {
		t.Errorf("selected %v, said %v", policy.Ports, warnings)
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
