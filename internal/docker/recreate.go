package docker

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/danielbodart/tidewaiter/internal/model"
)

// RollbackName is the one throwaway name an old container is parked under
// during a swap.
//
// One fixed suffix rather than a timestamp, so a swap interrupted half-way
// leaves a predictable name the next pass (or an operator) can find and clean
// up, instead of a litter of unique corpses.
func RollbackName(name string) string {
	return name + "-tidewaiter-rollback"
}

// ImageDefaults is what an image's own config sets: what Docker fills into a
// container created from it wherever the container does not set its own.
type ImageDefaults struct {
	Env         []string
	Cmd         []string
	Entrypoint  []string
	Labels      map[string]string
	Exposed     []string
	User        string
	WorkingDir  string
	StopSignal  string
	Healthcheck *model.Healthcheck
	Volumes     []string
}

// WithoutImageDefaults is spec with what it only has from its image taken
// back out, so a create on a newer image gets that image's defaults instead.
//
// Inspect shows a container's config already merged with its image's: Docker
// filled every field the container left unset from the image at create, and
// does not say which were which. Copied as it stands, a recreate would pin
// the old image's CMD, ENV, labels, HEALTHCHECK and the rest onto the new
// image for ever - a v2 still running v1's command. So whatever equals the
// old image's default is dropped, and Docker fills it from the new image
// exactly as it would for a fresh `docker run` of the container's own
// settings. Dropped per entry where Docker merges per entry (Env by
// variable, Labels by key, ExposedPorts and Volumes by path, Healthcheck by
// field), and whole elsewhere.
//
// A setting the user gave that happens to equal the old image's default is
// dropped too: the two cannot be told apart, and the new image's default is
// what a fresh `docker run` with that setting left out would get - the
// closest reading of a user who chose what the image already did.
//
// Anything a Docker merge would not have filled is kept whatever it equals,
// which matters in two places:
//
//   - Docker fills Cmd from the image only when the container's Entrypoint is
//     empty. A container that set an Entrypoint keeps exactly the Cmd it was
//     given, so its Cmd is its own even when it equals the image's, and
//     dropping it would hand the new image's CMD to an entrypoint that never
//     had one. The same rule tells an Entrypoint override that equals the
//     image's ENTRYPOINT from one inherited from it: only an override leaves
//     Cmd empty beside an image that has a CMD. (Watchtower drops such an
//     Entrypoint, and the new image's CMD then reaches it as arguments.)
//   - A port the container publishes stays exposed even when the image
//     exposes it too, so its binding still has a port to bind should the new
//     image stop exposing it (as Watchtower also does).
//
// An Entrypoint of [] is an explicit "none" (`"Entrypoint": []` on the
// API), kept unless the image's own is [] too; nil leaves it to the image.
func WithoutImageDefaults(spec model.ContainerSpec, image ImageDefaults) model.ContainerSpec {
	own := spec
	own.Env = []string{}
	for _, entry := range spec.Env {
		if !slices.Contains(image.Env, entry) {
			own.Env = append(own.Env, entry)
		}
	}
	own.Labels = orEmptyMap(spec.Labels)
	for key, value := range image.Labels {
		if own.Labels[key] == value {
			delete(own.Labels, key)
		}
	}
	published := map[string]bool{}
	for _, binding := range spec.Bindings {
		published[strconv.Itoa(binding.ContainerPort)+"/"+string(binding.Protocol)] = true
	}
	own.Exposed = []string{}
	for _, port := range spec.Exposed {
		if published[port] || !slices.Contains(image.Exposed, port) {
			own.Exposed = append(own.Exposed, port)
		}
	}

	ownEntrypoint := !sameList(spec.Entrypoint, image.Entrypoint) || (len(spec.Cmd) == 0 && len(image.Cmd) > 0)
	if !ownEntrypoint {
		own.Entrypoint = nil
	}
	// Cmd and the image's are compared by content alone: Docker reads an
	// empty Cmd as unset either way.
	if !(ownEntrypoint && len(spec.Entrypoint) > 0) && slices.Equal(spec.Cmd, image.Cmd) {
		own.Cmd = nil
	}

	if own.User == image.User {
		own.User = ""
	}
	if own.WorkingDir == image.WorkingDir {
		own.WorkingDir = ""
	}
	if own.StopSignal == image.StopSignal {
		own.StopSignal = ""
	}
	own.Healthcheck = ownHealthcheck(spec.Healthcheck, image.Healthcheck)
	own.Volumes = []string{}
	for _, volume := range spec.Volumes {
		if !slices.Contains(image.Volumes, volume) {
			own.Volumes = append(own.Volumes, volume)
		}
	}
	return own
}

// ownHealthcheck is a container's HEALTHCHECK less every field equal to its
// image's - Docker fills each unset field from the image separately, so an
// override of the interval alone must not keep the old image's test - or nil
// when nothing of its own is left.
func ownHealthcheck(check, image *model.Healthcheck) *model.Healthcheck {
	if check == nil || image == nil {
		return check
	}
	own := *check
	if slices.Equal(own.Test, image.Test) {
		own.Test = nil
	}
	if own.Interval == image.Interval {
		own.Interval = 0
	}
	if own.Timeout == image.Timeout {
		own.Timeout = 0
	}
	if own.Retries == image.Retries {
		own.Retries = 0
	}
	if own.StartPeriod == image.StartPeriod {
		own.StartPeriod = 0
	}
	if own.StartInterval == image.StartInterval {
		own.StartInterval = 0
	}
	if own.Unset() {
		return nil
	}
	return &own
}

// sameList is whether two lists say the same, null and [] told apart.
func sameList(a, b []string) bool {
	return (a == nil) == (b == nil) && slices.Equal(a, b)
}

// CreateBody is the /containers/create body that recreates spec's
// container, in Engine API version: what docker inspect said of it, passed
// back as it said it, less two short lists.
//
// Docker reports a container's Config, HostConfig and endpoint settings in
// the form the version it was asked in takes back, and inspect and create
// speak the same negotiated version. So passing them through carries every
// setting - limits, ulimits, logging, security options, DNS, mounts and
// their options, and whatever Docker adds next - with no list of fields to
// keep up to date. What does not pass through is on one of the two lists
// below, and nothing else is touched:
//
//   - overrides: what a recreate changes on purpose;
//   - removals: what inspect reports but create, in the same version,
//     rejects or misreads.
//
// spec is the container's own settings (WithoutImageDefaults), the typed
// fields saying what the overrides set; spec.Inspected is what it passes
// through.
func CreateBody(spec model.ContainerSpec, version string) (map[string]any, error) {
	inspected := spec.Inspected
	if inspected == nil {
		return nil, errors.New("no inspect of it to recreate it from")
	}
	config, host := passed(inspected.Config), passed(inspected.HostConfig)
	overrideConfig(config, spec)
	overrideHost(host)
	for _, removal := range removals {
		if removal.applies(host, version) {
			removal.from(config, host)
		}
	}
	body := config
	body["HostConfig"] = host

	delete(config, "MacAddress")
	if len(spec.Networks) > 0 {
		primary := spec.Networks[0].Name
		body["NetworkingConfig"] = map[string]any{"EndpointsConfig": map[string]any{primary: endpointBody(inspected, primary, true, version)}}
		// Before the endpoint took one, a MAC address was the container's
		// own, and went on its own network.
		if mac := macAddress(inspected, primary, true); mac != "" && !atLeast(version, apiEndpointMAC) {
			config["MacAddress"] = mac
		}
	}
	return body, nil
}

// ConnectBody is the /networks/{name}/connect body that attaches spec's
// container, by now created as container, to network as it was attached.
func ConnectBody(spec model.ContainerSpec, container, network, version string) map[string]any {
	endpoint := map[string]any{}
	if spec.Inspected != nil {
		endpoint = endpointBody(spec.Inspected, network, false, version)
	}
	return map[string]any{"Container": container, "EndpointConfig": endpoint}
}

// passed is fields copied for a body, each value still as Docker wrote it.
func passed(fields map[string]json.RawMessage) map[string]any {
	body := make(map[string]any, len(fields))
	for key, value := range fields {
		body[key] = value
	}
	return body
}

// Overrides: what a recreate changes on purpose. Everything else in Config
// and HostConfig passes through.
//
//   - Image: the ref the container was created from, which is the same tag;
//     what it resolves to is the update.
//   - Env, Cmd, Entrypoint, Labels, ExposedPorts, Volumes, User, WorkingDir,
//     StopSignal, Healthcheck: what moby's merge fills from the image at
//     create (daemon/commit.go, merge), so inspect shows them merged.
//     They are set to the container's own (WithoutImageDefaults), and left
//     out where it has none, so the new image's take effect; a healthcheck
//     keeps any field Tidewaiter does not know of, less the ones the old
//     image gave it.
//   - Hostname, when it is Docker's default, the old container's short id
//     (daemon/container.go): passed back, the new container would be named
//     after the old one.
//   - MacAddress: see endpointBody.
//   - HostConfig MaskedPaths and ReadonlyPaths, when they list anything:
//     Docker fills its defaults in at create when they are unset and the
//     container is not privileged (daemon/create_unix.go), so inspect shows
//     them set; passed back, they would pin this Docker's defaults over a
//     later one's. Empty, they are `--security-opt systempaths=unconfined`,
//     which the docker CLI sends as exactly that, and pass through. (A list
//     of one's own, which only the raw API can set, is the one thing lost.)
func overrideConfig(config map[string]any, spec model.ContainerSpec) {
	config["Image"] = spec.Image
	config["Env"] = orEmpty(spec.Env)
	config["Labels"] = orEmptyMap(spec.Labels)
	setOr(config, "Cmd", spec.Cmd, spec.Cmd != nil)
	setOr(config, "Entrypoint", spec.Entrypoint, spec.Entrypoint != nil)
	setOr(config, "ExposedPorts", set(spec.Exposed), len(spec.Exposed) > 0)
	setOr(config, "Volumes", set(spec.Volumes), len(spec.Volumes) > 0)
	setOr(config, "User", spec.User, spec.User != "")
	setOr(config, "WorkingDir", spec.WorkingDir, spec.WorkingDir != "")
	setOr(config, "StopSignal", spec.StopSignal, spec.StopSignal != "")
	setOr(config, "Healthcheck", healthcheckBody(config["Healthcheck"], spec.Healthcheck), spec.Healthcheck != nil)

	var hostname string
	if raw, ok := config["Hostname"].(json.RawMessage); ok && json.Unmarshal(raw, &hostname) == nil &&
		len(spec.Inspected.ID) >= 12 && hostname == spec.Inspected.ID[:12] {
		delete(config, "Hostname")
	}
}

func overrideHost(host map[string]any) {
	for _, key := range []string{"MaskedPaths", "ReadonlyPaths"} {
		var paths []string
		if raw, ok := host[key].(json.RawMessage); ok && json.Unmarshal(raw, &paths) == nil && len(paths) > 0 {
			delete(host, key)
		}
	}
}

// healthcheckBody is the container's own healthcheck: as inspect gave it,
// less each field own leaves to the image.
func healthcheckBody(inspected any, own *model.Healthcheck) any {
	if own == nil {
		return nil
	}
	fields := map[string]json.RawMessage{}
	if raw, ok := inspected.(json.RawMessage); ok {
		json.Unmarshal(raw, &fields)
	}
	body := passed(fields)
	for key, unset := range map[string]bool{
		"Test": len(own.Test) == 0, "Interval": own.Interval == 0, "Timeout": own.Timeout == 0,
		"Retries": own.Retries == 0, "StartPeriod": own.StartPeriod == 0, "StartInterval": own.StartInterval == 0,
	} {
		if unset {
			delete(body, key)
		}
	}
	return body
}

// Endpoint overrides: of a network's endpoint settings, the configuration
// passes through and the operational data - as moby's EndpointSettings
// itself divides them - does not: where the container is attached now,
// which a new endpoint is given afresh.
var operational = []string{
	"NetworkID", "EndpointID", "Gateway", "IPAddress", "IPPrefixLen",
	"IPv6Gateway", "GlobalIPv6Address", "GlobalIPv6PrefixLen", "DNSNames", "MacAddress",
}

// endpointBody is the settings that attach the container to network, at
// create (the container's own network) or connect.
//
// A MAC address is operational once the container runs, so it goes back
// only where inspect can say it is the container's (macAddress): on the
// endpoint from 1.44 at create, but only from 1.54 at connect, the first
// that applies it there; below 1.44 CreateBody puts it in Config.
//
// Before 1.45, inspect adds the container's short id to its aliases on a
// user-defined network (daemon/server/router/container/inspect.go); passed
// back, the old container's id would be an alias of the new one.
func endpointBody(inspected *model.Inspect, network string, create bool, version string) map[string]any {
	endpoint := passed(inspected.Endpoints[network])
	for _, key := range operational {
		delete(endpoint, key)
	}
	since := apiConnectMAC
	if create {
		since = apiEndpointMAC
	}
	if mac := macAddress(inspected, network, create); mac != "" && atLeast(version, since) {
		endpoint["MacAddress"] = mac
	}
	if raw, ok := endpoint["Aliases"].(json.RawMessage); ok && !atLeast(inspected.APIVersion, apiAliasesWithoutID) && len(inspected.ID) >= 12 {
		var aliases []string
		if json.Unmarshal(raw, &aliases) == nil {
			endpoint["Aliases"] = slices.DeleteFunc(aliases, func(alias string) bool { return alias == inspected.ID[:12] })
		}
	}
	return endpoint
}

// macAddress is the MAC address to keep on network, or "".
//
// Before API 1.52 inspect shows the one asked for on the container's own
// network as Config.MacAddress, and only when one was; a network's own
// MacAddress is then the address in use, which older Dockers derive from
// the IP, so pinning it could later clash with another container's. From
// 1.52 the network's MacAddress is all there is, and Docker 29, the first
// to speak 1.52, picks a random one for each endpoint, so keeping it is
// safe.
func macAddress(inspected *model.Inspect, network string, own bool) string {
	var mac string
	switch {
	case atLeast(inspected.APIVersion, apiInspectEndpointMAC):
		json.Unmarshal(inspected.Endpoints[network]["MacAddress"], &mac)
	case own:
		json.Unmarshal(inspected.Config["MacAddress"], &mac)
	}
	return mac
}

// Removals: what inspect reports that create rejects or misreads in the same
// version, each saying which Docker versions do so and why, so it can go once
// they have aged out. Found by recreating real containers
// (real_roundtrip_test.go), not guessed at.
var removals = []removal{
	{
		// A container sharing another's network (container:<x>) shows that
		// container's hostname, which Docker copies over at start
		// (daemon/container_operations.go), and create refuses any hostname
		// at all in that mode: "conflicting options: hostname and the network
		// mode" (daemon/internal/runconfig/hostconfig.go,
		// validateNetContainerMode). Every Docker so far.
		what: "Config.Hostname in container network mode",
		applies: func(host map[string]any, _ string) bool {
			var mode string
			raw, _ := host["NetworkMode"].(json.RawMessage)
			json.Unmarshal(raw, &mode)
			return strings.HasPrefix(mode, "container:")
		},
		from: func(config, _ map[string]any) { delete(config, "Hostname") },
	},
}

// removal is one field taken out of a create body, when it applies.
type removal struct {
	what    string
	applies func(host map[string]any, version string) bool
	from    func(config, host map[string]any)
}

// The Engine API versions the overrides above turn on.
const (
	// EndpointSettings.MacAddress on create; before it, Config.MacAddress,
	// which 1.52 no longer takes.
	apiEndpointMAC = "1.44"
	// Inspect stops adding the short id to a network's aliases.
	apiAliasesWithoutID = "1.45"
	// Inspect drops Config.MacAddress, leaving each network's MacAddress.
	apiInspectEndpointMAC = "1.52"
	// Connect applies EndpointSettings.MacAddress; it took and ignored it
	// from 1.44.
	apiConnectMAC = "1.54"
)

// atLeast is whether version is since or newer.
func atLeast(version, since string) bool {
	return compareVersions(version, since) >= 0
}

// setOr sets key to value when ok, and leaves it out otherwise.
func setOr(into map[string]any, key string, value any, ok bool) {
	if ok {
		into[key] = value
	} else {
		delete(into, key)
	}
}

// set is keys as Docker writes a set: a map to empty objects.
func set(keys []string) map[string]struct{} {
	found := map[string]struct{}{}
	for _, key := range keys {
		found[key] = struct{}{}
	}
	return found
}

// ExtraNetworks is every network beyond the first, which a swap connects
// after create.
func ExtraNetworks(spec model.ContainerSpec) []model.NetworkAttachment {
	if len(spec.Networks) <= 1 {
		return nil
	}
	return spec.Networks[1:]
}

// Unrecreatable is why a spec cannot be faithfully recreated, or "" when it
// can.
//
// An anonymous volume cannot: it has no name to reattach, so recreating would
// mint a fresh empty volume and silently drop the data. Rather than do that,
// the swap is refused and says why - the container keeps running its current
// image, which is the safe outcome.
func Unrecreatable(spec model.ContainerSpec) string {
	for _, mount := range spec.Mounts {
		if mount.Anonymous {
			return fmt.Sprintf("has an anonymous volume at %s, which cannot be recreated without "+
				"losing its data; deferring - give it a named volume to enable updates", mount.Target)
		}
	}
	return ""
}

// Op is one Docker call of a swap.
type Op string

const (
	Stop    Op = "stop"
	Rename  Op = "rename"
	Create  Op = "create"
	Connect Op = "connect"
	Start   Op = "start"
	Remove  Op = "remove"
)

// Step is one Docker call a swap makes, as data. Which fields mean anything
// depends on Op: Grace for stop, To for rename, Spec for create, Network and
// Spec for connect, Force for remove; Container for all but create.
type Step struct {
	Op        Op
	Container string
	Grace     int
	To        string
	Spec      model.ContainerSpec
	Network   model.NetworkAttachment
	Force     bool
}

// Plan is the ordered Docker calls that carry out a swap.
type Plan struct {
	// Stop the old container, park it, and stand the new one up in its place.
	Swap []Step
	// After a healthy new container: drop the parked old one.
	Commit []Step
	// After an unhealthy new container: drop the new one, restore the old.
	// Kept to document the shape; the source undoes a swap from how far it
	// actually got, never by walking this blindly.
	Rollback []Step
}

// PlanSwap is the swap from the running container to newSpec, as data.
//
// Returned as a plan rather than executed, so the sequencing - the subtle
// part - is testable with no Docker. The shape is rename-not-delete: the old
// container is parked under RollbackName rather than removed, so at every
// moment exactly one real container wears the original name and there is
// never a gap where it resolves to nothing. Commit removes the parked old one;
// rollback removes the new one and gives the old its name back. The new
// container is referred to by the original name throughout, which is
// unambiguous because the old one has been renamed away first.
func PlanSwap(spec, newSpec model.ContainerSpec, grace int) Plan {
	name, parked := spec.Name, RollbackName(spec.Name)
	swap := []Step{
		{Op: Stop, Container: name, Grace: grace},
		{Op: Rename, Container: name, To: parked},
		{Op: Create, Spec: newSpec},
	}
	for _, network := range ExtraNetworks(newSpec) {
		swap = append(swap, Step{Op: Connect, Container: name, Network: network, Spec: newSpec})
	}
	swap = append(swap, Step{Op: Start, Container: name})
	return Plan{
		Swap:   swap,
		Commit: []Step{{Op: Remove, Container: parked, Force: true}},
		Rollback: []Step{
			{Op: Remove, Container: name, Force: true},
			{Op: Rename, Container: parked, To: name},
			{Op: Start, Container: name},
		},
	}
}

func orEmptyMap(values map[string]string) map[string]string {
	if values == nil {
		return map[string]string{}
	}
	return maps.Clone(values)
}
