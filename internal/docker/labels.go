package docker

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/danielbodart/tidewaiter/internal/model"
)

// The labels Tidewaiter reads off a container, in Portical's
// `portical.upnp.*` style so the two read as a family.
//
// Autoupdate is the opt-in, Podman style: its *value* is the update policy,
// and a container is managed only if it carries the label with a value we
// recognise. There is no separate boolean.
const (
	AutoupdateLabel    = "tidewaiter.autoupdate"
	DetectorLabel      = "tidewaiter.detector"
	PortsLabel         = "tidewaiter.ports"
	IdleSamplesLabel   = "tidewaiter.idle-samples"
	HealthLabel        = "tidewaiter.health"
	HealthTimeoutLabel = "tidewaiter.health-timeout"
	GraceLabel         = "tidewaiter.grace"
	KeepImagesLabel    = "tidewaiter.keep-images"
)

// Autoupdate is an update policy, as Podman's io.containers.autoupdate:
// Registry compares the tag against the registry and pulls when the digest
// moved; Local compares it against the locally stored image, with no
// registry contact, and recreates when a build or pull changed it.
type Autoupdate string

const (
	Registry Autoupdate = "registry"
	Local    Autoupdate = "local"
)

var (
	policies  = []string{string(Registry), string(Local)}
	detectors = []string{"conntrack", "tcp", "netio", "none"}
	checks    = []string{"docker", "port-connect", "port-bound", "uptime"}
)

// Policy is a container's resolved Tidewaiter policy.
//
// Only ever built for a container that is validly opted in, so Autoupdate is
// always a real policy. The rest have defaults, so a container with only
// `tidewaiter.autoupdate=registry` gets a complete, sensible policy.
type Policy struct {
	Autoupdate Autoupdate
	Detector   string
	// Ports scopes the activity gate and the health probe, when PortsOverride
	// says tidewaiter.ports was given - otherwise every published port is in
	// scope, the right default.
	//
	// The override exists for a real case: a container that publishes a
	// service port *and* one that gets constant automated traffic - a
	// Prometheus endpoint scraped every few seconds, an admin port a load
	// balancer polls. Those keep live flows open the whole time, so the gate
	// would read the container busy forever and it would never update. An
	// override that selected nothing is kept as such: the source cannot tell
	// what to watch, so reads it as busy.
	Ports         []model.PublishedPort
	PortsOverride bool
	IdleSamples   int
	// The checks that gate the swap, combined: the gate rolls back only when
	// one ACTIVELY fails (health.Combine), because false-unhealthy is the
	// worse error. Each covers another's blind spot - docker tests the app's
	// insides; port-connect connects to the container's own address, catching
	// a service bound to the wrong one; port-bound reads the netns for a
	// listening socket, the only UDP signal and the one that sees past
	// docker-proxy's fake accept; uptime catches an exit or crash loop.
	Health               []string
	HealthTimeoutSeconds int
	// nil is "the container's own stop timeout".
	GraceSeconds *int
	KeepImages   int
}

// Defaults are what every setting but the opt-in falls back to.
var Defaults = Policy{
	Detector:             "conntrack",
	IdleSamples:          3,
	Health:               checks,
	HealthTimeoutSeconds: 120,
	KeepImages:           1,
}

// ParseLabels reads a container's labels into a policy, with the problems as
// data.
//
// optIn is the opt-in label's key - tidewaiter.autoupdate unless --label
// names another. Opt-in is decided first and decisively: without that
// label whose value is a known policy, the policy is nil and the container is
// not managed at all - never inspected for activity, never updated. An
// unrecognised value is a warning (a typo like `registy` should say so, not
// silently do nothing).
//
// Once opted in, the rest are parsed with defaults and warnings returned
// rather than thrown, as Portical's labels are: one misconfigured setting must
// not stop the container being managed with the default for it. published is
// what the container actually publishes, to check a tidewaiter.ports override
// against.
func ParseLabels(optIn string, labels map[string]string, published []model.PublishedPort) (*Policy, []string) {
	value, present := labels[optIn]
	if !present {
		// Silent: the common case for most containers on a host.
		return nil, nil
	}
	autoupdate := strings.ToLower(strings.TrimSpace(value))
	if !slices.Contains(policies, autoupdate) {
		// Opted in by the key, but not to anything we know: never update on an
		// ambiguous instruction.
		return nil, []string{fmt.Sprintf("%s: '%s' is not a known policy (%s); ignoring this container",
			optIn, value, strings.Join(policies, ", "))}
	}

	var warnings []string
	policy := Defaults
	policy.Autoupdate = Autoupdate(autoupdate)
	policy.Health = slices.Clone(Defaults.Health)
	if value, ok := labels[DetectorLabel]; ok {
		if detector := strings.ToLower(strings.TrimSpace(value)); slices.Contains(detectors, detector) {
			policy.Detector = detector
		} else {
			warnings = append(warnings, fmt.Sprintf("%s: '%s' is not one of %s; using %s",
				DetectorLabel, value, strings.Join(detectors, ", "), Defaults.Detector))
		}
	}
	if value, ok := labels[HealthLabel]; ok {
		policy.Health = parseHealth(value, &warnings)
	}
	policy.IdleSamples = number(labels, IdleSamplesLabel, Defaults.IdleSamples, 1, &warnings)
	policy.HealthTimeoutSeconds = number(labels, HealthTimeoutLabel, Defaults.HealthTimeoutSeconds, 1, &warnings)
	policy.KeepImages = number(labels, KeepImagesLabel, Defaults.KeepImages, 0, &warnings)
	// A malformed grace falls back to the container's own stop timeout, never
	// to 0: that would be an immediate SIGKILL with no drain.
	if grace, ok := whole(labels, GraceLabel, 0, "the container's own stop timeout", &warnings); ok {
		policy.GraceSeconds = &grace
	}
	if value, ok := labels[PortsLabel]; ok {
		policy.Ports = selectPorts(value, published, &warnings)
		policy.PortsOverride = true
	}
	return &policy, warnings
}

// parseHealth reads a comma-separated tidewaiter.health list.
//
// Unknown names are warned about and dropped, keeping the rest. `port` (the
// old single value) is no longer a check; it means port-connect,port-bound so
// old labels keep meaning what they did. An empty or all-unknown list falls
// back to the default rather than leave a container with no gate.
func parseHealth(value string, warnings *[]string) []string {
	var chosen []string
	add := func(name string) {
		if !slices.Contains(chosen, name) {
			chosen = append(chosen, name)
		}
	}
	for raw := range strings.SplitSeq(value, ",") {
		term := strings.ToLower(strings.TrimSpace(raw))
		switch {
		case term == "":
		case term == "port":
			add("port-connect")
			add("port-bound")
		case slices.Contains(checks, term):
			add(term)
		default:
			*warnings = append(*warnings, fmt.Sprintf("%s: '%s' is not a known check (%s); ignoring it",
				HealthLabel, strings.TrimSpace(raw), strings.Join(checks, ", ")))
		}
	}
	if len(chosen) == 0 {
		return slices.Clone(Defaults.Health)
	}
	return chosen
}

// number is a whole-number label of at least least, or fallback with a
// warning. Strict: a decimal, an exponent or a blank is not a whole number.
func number(labels map[string]string, name string, fallback, least int, warnings *[]string) int {
	if parsed, ok := whole(labels, name, least, strconv.Itoa(fallback), warnings); ok {
		return parsed
	}
	return fallback
}

// whole is a whole-number label of at least least, and whether it was set to
// one; one set to anything else is warned about, saying the default it falls
// back to.
func whole(labels map[string]string, name string, least int, fallback string, warnings *[]string) (int, bool) {
	value, ok := labels[name]
	if !ok {
		return 0, false
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	switch {
	case err != nil:
		*warnings = append(*warnings, fmt.Sprintf("%s: '%s' is not a whole number; ignoring it", name, value))
		return 0, false
	case parsed < least && least == 0:
		*warnings = append(*warnings, fmt.Sprintf("%s: '%s' must not be negative; using %s", name, value, fallback))
		return 0, false
	case parsed < least:
		*warnings = append(*warnings, fmt.Sprintf("%s: '%s' must be at least %d; using %s", name, value, least, fallback))
		return 0, false
	}
	return parsed, true
}

// selectPorts resolves a tidewaiter.ports override against the container's
// real ports.
//
// A term with no protocol matches whichever the container publishes. A term
// naming a port the container does not publish is reported and dropped - it
// is almost always a typo, and silently watching a port nobody serves would
// read as "always idle" and update a live container.
func selectPorts(value string, published []model.PublishedPort, warnings *[]string) []model.PublishedPort {
	selected := []model.PublishedPort{}
	for raw := range strings.SplitSeq(value, ",") {
		term := strings.TrimSpace(raw)
		if term == "" {
			continue
		}
		port, protocol, err := ParsePortTerm(term)
		if err != nil {
			*warnings = append(*warnings, PortsLabel+": "+err.Error())
			continue
		}
		matched := false
		for _, candidate := range published {
			if candidate.HostPort == port && (protocol == "" || candidate.Protocol == protocol) {
				matched = true
				if !slices.ContainsFunc(selected, func(p model.PublishedPort) bool {
					return p.Protocol == candidate.Protocol && p.HostPort == candidate.HostPort
				}) {
					selected = append(selected, candidate)
				}
			}
		}
		if !matched {
			*warnings = append(*warnings, fmt.Sprintf("%s: '%s' is not a port this container publishes; ignoring it", PortsLabel, term))
		}
	}
	return selected
}

var portTerm = regexp.MustCompile(`(?i)^(\d{1,5})(?:/(tcp|udp))?$`)

// ParsePortTerm reads one `port` or `port/proto` term, as tidewaiter.ports
// writes it; protocol "" means the term named none. The error is the reason
// it was not one, ready to warn with.
func ParsePortTerm(term string) (int, model.Protocol, error) {
	match := portTerm.FindStringSubmatch(term)
	if match == nil {
		return 0, "", fmt.Errorf("ignoring unrecognised port '%s'", term)
	}
	port, _ := strconv.Atoi(match[1])
	if port < 1 || port > 65535 {
		return 0, "", fmt.Errorf("ignoring '%s', ports must be between 1 and 65535", term)
	}
	return port, model.Protocol(strings.ToLower(match[2])), nil
}
