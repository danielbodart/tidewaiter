package docker

import (
	"reflect"
	"testing"
	"time"

	"github.com/danielbodart/tidewaiter/internal/decide"
	"github.com/danielbodart/tidewaiter/internal/model"
)

// recreated is a swap of a container set up by change, with Docker speaking
// API versions up to version: the container it created, as Docker shows it,
// the create body it sent, and the networks it connected after.
type recreated struct {
	spec     model.ContainerSpec
	sent     map[string]any
	connects []connected
}

func recreateAt(t *testing.T, version string, change func(*model.ContainerSpec)) recreated {
	t.Helper()
	w := newWorld(t)
	w.docker.api.speaks("1.24", version)
	w.docker.add(container("web", func(c *model.RunningContainer) {
		c.ImageID = "config-sha256:old"
		c.Spec.Labels[IdleSamplesLabel] = "1"
		c.Spec.Labels[HealthLabel] = "docker"
		c.Spec.Networks = []model.NetworkAttachment{{Name: "front", Aliases: []string{}}}
		c.Spec.NetworkMode = "front"
		change(&c.Spec)
	}))
	w.registry.set("app:latest", "sha256:new")
	w.docker.with(func(f *fakeDocker) {
		f.images["config-sha256:old"] = "sha256:old"
		f.pullLands["app:latest"] = "sha256:new"
		f.healthByName["web"] = model.Healthy
	})

	w.expect(t, decide.Update)

	return read(w.docker, func(f *fakeDocker) recreated {
		if len(f.creates) != 1 {
			t.Fatalf("created %d containers at v%s: %s", len(f.creates), version, w.log.Text())
		}
		return recreated{spec: f.creates[0], sent: f.payloads[0], connects: f.connects}
	})
}

// Each field a recreate carries that is newer than the API it was written
// for: kept at the version that brought it in, and at the one before, left
// out of the create - which still goes ahead - as it was before Tidewaiter
// knew it. The fake Docker refuses a field its version does not know, so a
// create that goes ahead sent none.
func TestARecreateCarriesEachNewerFieldOnlyInAVersionThatKnowsIt(t *testing.T) {
	interval := time.Second
	umask := uint32(0o027)
	for _, c := range []struct {
		field         string
		since, before string
		set           func(*model.ContainerSpec)
		kept          func(model.ContainerSpec) bool
	}{
		{"Healthcheck.StartInterval", apiStartInterval, "1.43",
			func(s *model.ContainerSpec) {
				s.Healthcheck = &model.Healthcheck{Test: []string{"CMD", "true"}, StartPeriod: time.Minute, StartInterval: interval}
			},
			func(s model.ContainerSpec) bool {
				return s.Healthcheck != nil && s.Healthcheck.StartInterval == interval
			}},
		{"an image mount", apiImageMount, "1.47",
			func(s *model.ContainerSpec) {
				s.Mounts = []model.Mount{{Type: model.Image, Source: "tools:1", Target: "/tools", ReadOnly: true, Image: model.ImageOptions{Subpath: "bin"}}}
			},
			func(s model.ContainerSpec) bool {
				return reflect.DeepEqual(s.Mounts, []model.Mount{{Type: model.Image, Source: "tools:1", Target: "/tools", ReadOnly: true, Image: model.ImageOptions{Subpath: "bin"}}})
			}},
		{"BindOptions.NonRecursive", apiBindNonRecursive, "1.39",
			longBind(model.BindOptions{NonRecursive: true}),
			func(s model.ContainerSpec) bool { return s.Mounts[0].Bind.NonRecursive }},
		{"BindOptions.CreateMountpoint", apiCreateMountpoint, "1.41",
			longBind(model.BindOptions{CreateMountpoint: true}),
			func(s model.ContainerSpec) bool { return s.Mounts[0].Bind.CreateMountpoint }},
		{"BindOptions.ReadOnlyNonRecursive", apiReadOnlyRecursive, "1.43",
			longBind(model.BindOptions{ReadOnlyNonRecursive: true}),
			func(s model.ContainerSpec) bool { return s.Mounts[0].Bind.ReadOnlyNonRecursive }},
		{"BindOptions.ReadOnlyForceRecursive", apiReadOnlyRecursive, "1.43",
			longBind(model.BindOptions{ReadOnlyForceRecursive: true}),
			func(s model.ContainerSpec) bool { return s.Mounts[0].Bind.ReadOnlyForceRecursive }},
		{"VolumeOptions.Subpath", apiVolumeSubpath, "1.44",
			func(s *model.ContainerSpec) {
				s.Mounts = []model.Mount{{Type: model.Volume, Source: "data", Target: "/data", Volume: model.VolumeOptions{Subpath: "app"}}}
			},
			func(s model.ContainerSpec) bool { return s.Mounts[0].Volume.Subpath == "app" }},
		{"TmpfsOptions.Options", apiTmpfsOptions, "1.45",
			func(s *model.ContainerSpec) {
				s.Mounts = []model.Mount{{Type: model.Tmpfs, Target: "/scratch", Tmpfs: model.TmpfsOptions{Options: [][]string{{"noexec"}}}}}
			},
			func(s model.ContainerSpec) bool {
				return reflect.DeepEqual(s.Mounts[0].Tmpfs.Options, [][]string{{"noexec"}})
			}},
		{"EndpointSettings.GwPriority", apiGwPriority, "1.47",
			func(s *model.ContainerSpec) { s.Networks[0].GwPriority = 10 },
			func(s model.ContainerSpec) bool { return s.Networks[0].GwPriority == 10 }},
		{"HostConfig.Annotations", apiAnnotations, "1.42",
			func(s *model.ContainerSpec) { s.Annotations = map[string]string{"io.example/owner": "ops"} },
			func(s model.ContainerSpec) bool { return s.Annotations["io.example/owner"] == "ops" }},
		{"HostConfig.Umask", apiUmask, "1.55",
			func(s *model.ContainerSpec) { s.Umask = &umask },
			func(s model.ContainerSpec) bool { return s.Umask != nil && *s.Umask == umask }},
	} {
		t.Run(c.field, func(t *testing.T) {
			// The create on its own, whatever inspect showed: sent in the
			// version that knows it, which the one before would refuse, and
			// not sent in that one.
			spec := spec("web")
			spec.Networks = []model.NetworkAttachment{{Name: "front"}}
			c.set(&spec)
			if field := tooNew([]byte(jsonOf(CreatePayload(spec, c.since))), c.before); field == "" {
				t.Errorf("not sent at v%s", c.since)
			}
			if field := tooNew([]byte(jsonOf(CreatePayload(spec, c.before))), c.before); field != "" {
				t.Errorf("sent %s at v%s", field, c.before)
			}
			if got := recreateAt(t, c.since, c.set); !c.kept(got.spec) {
				t.Errorf("lost at v%s: %+v", c.since, got.spec)
			}
			if got := recreateAt(t, c.before, c.set); c.kept(got.spec) {
				t.Errorf("kept at v%s: %+v", c.before, got.spec)
			}
		})
	}
}

// longBind is a container with one bind made with --mount and options.
func longBind(options model.BindOptions) func(*model.ContainerSpec) {
	return func(s *model.ContainerSpec) {
		s.Mounts = []model.Mount{{Type: model.Bind, Source: "/srv", Target: "/srv", LongForm: true, Bind: options}}
	}
}

// What needs nothing newer than the API it was written for is carried at
// any version: a tmpfs's size and mode, a volume's nocopy, a bind's
// propagation whichever way it was made, --tmpfs, and link-local addresses.
func TestARecreateCarriesMountOptionsAndTmpfsThatEveryVersionKnows(t *testing.T) {
	set := func(s *model.ContainerSpec) {
		s.Mounts = []model.Mount{
			{Type: model.Bind, Source: "/a", Target: "/a", Bind: model.BindOptions{Propagation: "rslave"}},
			{Type: model.Bind, Source: "/b", Target: "/b", ReadOnly: true, LongForm: true, Bind: model.BindOptions{Propagation: "rshared"}},
			{Type: model.Volume, Source: "data", Target: "/data", Volume: model.VolumeOptions{NoCopy: true}},
			{Type: model.Tmpfs, Target: "/scratch", Tmpfs: model.TmpfsOptions{SizeBytes: 64 << 20, Mode: 0o1777}},
		}
		s.Tmpfs = map[string]string{"/run": "size=8m"}
		s.Networks[0].LinkLocalIPs = []string{"169.254.0.5"}
	}
	for _, version := range []string{"1.41", NewestAPI} {
		got := recreateAt(t, version, set)
		want := model.ContainerSpec{Networks: []model.NetworkAttachment{{}}}
		set(&want)
		if !reflect.DeepEqual(got.spec.Mounts, want.Mounts) || !reflect.DeepEqual(got.spec.Tmpfs, want.Tmpfs) ||
			!reflect.DeepEqual(got.spec.Networks[0].LinkLocalIPs, []string{"169.254.0.5"}) {
			t.Errorf("v%s: got %+v", version, got.spec)
		}
		// A -v bind stays a -v bind, its propagation in its mode.
		expectJSON(t, "Binds", got.sent["HostConfig"].(map[string]any)["Binds"], `["/a:/a:rslave"]`)
	}
}

// Where the MAC address goes moved at 1.44, from the container's Config to
// its network's endpoint, and 1.52 stopped taking it in Config at all; inspect
// shows the one asked for in Config until 1.52, and from then only on each
// network.
func TestARecreateKeepsTheContainersMacAddressWhereItsVersionTakesIt(t *testing.T) {
	const mac = "02:00:00:aa:bb:cc"
	onItsNetwork := func(s *model.ContainerSpec) { s.Networks[0].MacAddress = mac }
	for _, c := range []struct {
		version    string
		inConfig   bool
		onEndpoint bool
	}{
		{"1.43", true, false},
		{apiEndpointMAC, false, true},
		{"1.51", false, true},
		{apiInspectEndpointMAC, false, true},
		{NewestAPI, false, true},
	} {
		got := recreateAt(t, c.version, onItsNetwork)
		if got.spec.Networks[0].MacAddress != mac {
			t.Errorf("v%s: lost the MAC address: %+v", c.version, got.spec.Networks)
		}
		_, inConfig := got.sent["MacAddress"]
		endpoint := got.sent["NetworkingConfig"].(map[string]any)["EndpointsConfig"].(map[string]any)["front"].(map[string]any)
		_, onEndpoint := endpoint["MacAddress"]
		if inConfig != c.inConfig || onEndpoint != c.onEndpoint {
			t.Errorf("v%s: sent in Config %v, on the endpoint %v", c.version, inConfig, onEndpoint)
		}
	}
}

// A network connected after the create keeps its MAC address from 1.54,
// the first connect that applies one, and from 1.52, the first inspect that
// says which is the container's on a network other than its own.
func TestAConnectedNetworkKeepsItsMacAddressFromTheVersionThatAppliesIt(t *testing.T) {
	const mac = "02:00:00:dd:ee:ff"
	twoNetworks := func(s *model.ContainerSpec) {
		s.Networks = append(s.Networks, model.NetworkAttachment{Name: "back", Aliases: []string{}, MacAddress: mac, GwPriority: 5})
	}
	for _, c := range []struct {
		version  string
		mac      string
		priority int
	}{
		{"1.47", "", 0},
		{"1.51", "", 5},
		{"1.53", "", 5},
		{apiConnectMAC, mac, 5},
	} {
		got := recreateAt(t, c.version, twoNetworks)
		if len(got.connects) != 1 || got.connects[0].network.MacAddress != c.mac || got.connects[0].network.GwPriority != c.priority {
			t.Errorf("v%s: connected %+v", c.version, got.connects)
		}
	}
}

// Inspect's MAC address is the container's own only where the version says
// so: before 1.52 a network's MacAddress is the one in use, picked by Docker
// unless Config.MacAddress says otherwise, and is not pinned.
func TestSpecReadsTheMacAddressTheContainerAskedFor(t *testing.T) {
	networks := map[string]InspectedNetwork{"front": {MacAddress: "02:42:ac:11:00:02"}, "back": {MacAddress: "02:42:ac:12:00:02"}}
	for _, c := range []struct {
		version, config string
		want            []string
	}{
		{"1.51", "", []string{"", ""}},
		{"1.51", "02:00:00:aa:bb:cc", []string{"02:00:00:aa:bb:cc", ""}},
		{"1.52", "", []string{"02:42:ac:11:00:02", "02:42:ac:12:00:02"}},
	} {
		attached := networksOf(networks, "front", c.version, c.config)
		if got := []string{attached[0].MacAddress, attached[1].MacAddress}; !reflect.DeepEqual(got, c.want) || attached[0].Name != "front" {
			t.Errorf("v%s with Config.MacAddress %q: got %v", c.version, c.config, got)
		}
	}
}
