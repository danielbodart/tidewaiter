package docker

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/danielbodart/tidewaiter/internal/model"
)

func both(port int, protocol model.Protocol) model.PublishedPort {
	return model.PublishedPort{HostPort: port, ContainerPort: port, Protocol: protocol}
}

// opted parses labels, opted in to the registry policy unless they say
// otherwise.
func opted(t *testing.T, labels map[string]string, published ...model.PublishedPort) (*Policy, []string) {
	t.Helper()
	all := map[string]string{AutoupdateLabel: "registry"}
	for key, value := range labels {
		all[key] = value
	}
	policy, warnings := ParseLabels(AutoupdateLabel, all, published)
	if policy == nil {
		t.Fatalf("not opted in: %v", warnings)
	}
	return policy, warnings
}

func TestAnUnlabelledContainerIsNotOptedInAndNothingIsSaid(t *testing.T) {
	if policy, warnings := ParseLabels(AutoupdateLabel, map[string]string{}, nil); policy != nil || len(warnings) != 0 {
		t.Errorf("got %+v, %v", policy, warnings)
	}
}

func TestRegistryOptsInWithEveryDefault(t *testing.T) {
	policy, warnings := opted(t, nil)
	want := Defaults
	want.Autoupdate = Registry
	if !reflect.DeepEqual(*policy, want) || len(warnings) != 0 {
		t.Errorf("got %+v, %v", *policy, warnings)
	}
}

func TestLocalOptsInWithTheLocalPolicy(t *testing.T) {
	if policy, _ := opted(t, map[string]string{AutoupdateLabel: "local"}); policy.Autoupdate != Local {
		t.Errorf("got %q", policy.Autoupdate)
	}
}

func TestAnUnrecognisedAutoupdateValueIsNotOptedInAndWarns(t *testing.T) {
	policy, warnings := ParseLabels(AutoupdateLabel, map[string]string{AutoupdateLabel: "registy"}, nil)
	if policy != nil || !slices.Equal(warnings, []string{"tidewaiter.autoupdate: 'registy' is not a known policy (registry, local); ignoring this container"}) {
		t.Errorf("got %+v, %v", policy, warnings)
	}
}

func TestTheAutoupdateValueIsCaseInsensitiveAndTrimmed(t *testing.T) {
	if policy, _ := opted(t, map[string]string{AutoupdateLabel: " Registry "}); policy.Autoupdate != Registry {
		t.Errorf("got %q", policy.Autoupdate)
	}
}

func TestReadsEveryOverride(t *testing.T) {
	policy, warnings := opted(t, map[string]string{
		DetectorLabel:      "tcp",
		HealthLabel:        "docker",
		IdleSamplesLabel:   "5",
		HealthTimeoutLabel: "60",
		GraceLabel:         "20",
		KeepImagesLabel:    "3",
	})
	if len(warnings) != 0 || policy.Detector != "tcp" || !slices.Equal(policy.Health, []string{"docker"}) || policy.IdleSamples != 5 ||
		policy.HealthTimeoutSeconds != 60 || policy.GraceSeconds == nil || *policy.GraceSeconds != 20 || policy.KeepImages != 3 {
		t.Errorf("got %+v, %v", *policy, warnings)
	}
}

func TestHealth(t *testing.T) {
	all := []string{"docker", "port-connect", "port-bound", "uptime"}
	for _, c := range []struct {
		name, value string
		want        []string
		warned      bool
	}{
		{"a comma-separated subset, deduplicated and in order", "uptime, docker ,uptime", []string{"uptime", "docker"}, false},
		{"an unknown check is dropped with a warning, keeping the rest", "docker,psychic", []string{"docker"}, true},
		{"the legacy port is port-connect and port-bound", "port", []string{"port-connect", "port-bound"}, false},
		{"an all-unknown list is the default rather than no gate", "nonsense", all, true},
		{"an empty list is the default", "", all, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			policy, warnings := opted(t, map[string]string{HealthLabel: c.value})
			if !slices.Equal(policy.Health, c.want) || (len(warnings) > 0) != c.warned {
				t.Errorf("got %v, %v", policy.Health, warnings)
			}
			if c.warned && !strings.Contains(warnings[0], "is not a known check") {
				t.Errorf("warned %v", warnings)
			}
		})
	}
	if policy, _ := opted(t, nil); !slices.Equal(policy.Health, all) {
		t.Errorf("default %v", policy.Health)
	}
}

func TestAnUnknownDetectorWarnsAndFallsBackToTheDefault(t *testing.T) {
	policy, warnings := opted(t, map[string]string{DetectorLabel: "psychic"})
	if policy.Detector != "conntrack" || !slices.Equal(warnings, []string{"tidewaiter.detector: 'psychic' is not one of conntrack, tcp, netio, none; using conntrack"}) {
		t.Errorf("got %q, %v", policy.Detector, warnings)
	}
}

// Go's strict reading: what is not plainly a whole number is not one.
func TestNumbersAreWholeAndInRangeOrTheDefaultWithAWarning(t *testing.T) {
	for _, c := range []struct {
		label, value string
		warning      string
	}{
		{IdleSamplesLabel, "lots", "tidewaiter.idle-samples: 'lots' is not a whole number; ignoring it"},
		{IdleSamplesLabel, "2.5", "tidewaiter.idle-samples: '2.5' is not a whole number; ignoring it"},
		{IdleSamplesLabel, "1e1", "tidewaiter.idle-samples: '1e1' is not a whole number; ignoring it"},
		{IdleSamplesLabel, "", "tidewaiter.idle-samples: '' is not a whole number; ignoring it"},
		{IdleSamplesLabel, "0", "tidewaiter.idle-samples: '0' must be at least 1; using 3"},
		{HealthTimeoutLabel, "0", "tidewaiter.health-timeout: '0' must be at least 1; using 120"},
		{KeepImagesLabel, "-1", "tidewaiter.keep-images: '-1' must not be negative; using 1"},
		{GraceLabel, "-5", "tidewaiter.grace: '-5' must not be negative; using the container's own stop timeout"},
		{GraceLabel, "30s", "tidewaiter.grace: '30s' is not a whole number; ignoring it"},
	} {
		policy, warnings := opted(t, map[string]string{c.label: c.value})
		if !slices.Equal(warnings, []string{c.warning}) {
			t.Errorf("%s=%q: warned %v", c.label, c.value, warnings)
		}
		if policy.IdleSamples != 3 || policy.HealthTimeoutSeconds != 120 || policy.KeepImages != 1 {
			t.Errorf("%s=%q: got %+v", c.label, c.value, *policy)
		}
	}
}

func TestKeepImagesMayBeZero(t *testing.T) {
	if policy, warnings := opted(t, map[string]string{KeepImagesLabel: "0"}); policy.KeepImages != 0 || len(warnings) != 0 {
		t.Errorf("got %d, %v", policy.KeepImages, warnings)
	}
}

func TestGraceAbsentIsTheContainersOwn(t *testing.T) {
	if policy, _ := opted(t, nil); policy.GraceSeconds != nil {
		t.Errorf("got %d", *policy.GraceSeconds)
	}
}

// Not 0: a typo must not turn a graceful stop into an immediate SIGKILL.
func TestAMalformedGraceIsTheContainersOwn(t *testing.T) {
	for _, value := range []string{"30s", "-5", ""} {
		if policy, _ := opted(t, map[string]string{GraceLabel: value}); policy.GraceSeconds != nil {
			t.Errorf("%q: got %d", value, *policy.GraceSeconds)
		}
	}
}

func TestAGraceOfZeroIsKeptAsAsked(t *testing.T) {
	if policy, warnings := opted(t, map[string]string{GraceLabel: "0"}); policy.GraceSeconds == nil || *policy.GraceSeconds != 0 || len(warnings) != 0 {
		t.Errorf("got %v, %v", policy.GraceSeconds, warnings)
	}
}

func TestPortsOverride(t *testing.T) {
	for _, c := range []struct {
		name, value string
		published   []model.PublishedPort
		want        []model.PublishedPort
		warning     string
	}{
		{"selects the matching published ports", "8080",
			[]model.PublishedPort{both(8080, model.TCP), both(9090, model.TCP)}, []model.PublishedPort{both(8080, model.TCP)}, ""},
		{"honours a protocol", "19132/udp",
			[]model.PublishedPort{both(19132, model.TCP), both(19132, model.UDP)}, []model.PublishedPort{both(19132, model.UDP)}, ""},
		{"a bare port matches whichever protocols are published", "19132",
			[]model.PublishedPort{both(19132, model.TCP), both(19132, model.UDP)}, []model.PublishedPort{both(19132, model.TCP), both(19132, model.UDP)}, ""},
		{"a bare port matches the one protocol published", "19132",
			[]model.PublishedPort{both(19132, model.UDP)}, []model.PublishedPort{both(19132, model.UDP)}, ""},
		{"comma-separated terms combine, case-insensitively", "25565/TCP, 19132/udp,25565",
			[]model.PublishedPort{both(25565, model.TCP), both(25565, model.UDP), both(19132, model.UDP)},
			[]model.PublishedPort{both(25565, model.TCP), both(19132, model.UDP), both(25565, model.UDP)}, ""},
		{"a port not published is dropped with a warning", "1234",
			[]model.PublishedPort{both(8080, model.TCP)}, []model.PublishedPort{}, "tidewaiter.ports: '1234' is not a port this container publishes; ignoring it"},
		{"a malformed term is dropped with a warning", "http",
			[]model.PublishedPort{both(8080, model.TCP)}, []model.PublishedPort{}, "tidewaiter.ports: ignoring unrecognised port 'http'"},
		{"an out-of-range port is dropped with a warning", "70000",
			[]model.PublishedPort{both(8080, model.TCP)}, []model.PublishedPort{}, "tidewaiter.ports: ignoring '70000', ports must be between 1 and 65535"},
	} {
		t.Run(c.name, func(t *testing.T) {
			policy, warnings := opted(t, map[string]string{PortsLabel: c.value}, c.published...)
			if !policy.PortsOverride || !reflect.DeepEqual(policy.Ports, c.want) {
				t.Errorf("got %v (override %v)", policy.Ports, policy.PortsOverride)
			}
			if (c.warning == "" && len(warnings) != 0) || (c.warning != "" && !slices.Equal(warnings, []string{c.warning})) {
				t.Errorf("warned %v", warnings)
			}
		})
	}
	if policy, _ := opted(t, nil, both(8080, model.TCP)); policy.PortsOverride {
		t.Error("no label is no override")
	}
}

// --label names the opt-in: its value is the policy, as tidewaiter.autoupdate's is.
func TestTheOptInIsWhateverLabelTheSourceReads(t *testing.T) {
	labels := map[string]string{"site.autoupdate": "local", AutoupdateLabel: "registry"}
	if policy, _ := ParseLabels("site.autoupdate", labels, nil); policy == nil || policy.Autoupdate != Local {
		t.Errorf("got %+v", policy)
	}
	_, warnings := ParseLabels("site.autoupdate", map[string]string{"site.autoupdate": "never"}, nil)
	if !slices.Equal(warnings, []string{"site.autoupdate: 'never' is not a known policy (registry, local); ignoring this container"}) {
		t.Errorf("warned %v", warnings)
	}
}
