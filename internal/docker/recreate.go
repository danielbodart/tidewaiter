package docker

import (
	"fmt"
	"maps"
	"slices"
	"strconv"

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
// variable, Labels by key, ExposedPorts by port, Healthcheck by field), and
// whole elsewhere.
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
	if own.Unset() {
		return nil
	}
	return &own
}

// sameList is whether two lists say the same, null and [] told apart.
func sameList(a, b []string) bool {
	return (a == nil) == (b == nil) && slices.Equal(a, b)
}

// CreatePayload is a faithful /containers/create body for a container.
//
// A hand-picked allowlist, deliberately NOT a deep copy of the inspect blob:
// inspect carries runtime-only state (the id, the current IP, the PID, State)
// that Docker would reject or misapply on create. Every field here is part of
// what the container *is*, not of what it is *doing right now*. The recreate
// tests are the living checklist - an inspect field that should survive a
// recreate is a failing test until it is added here.
//
// A loose map on purpose: it is JSON handed straight to Docker, and a struct
// for every nested field would only copy Docker's own schema. What matters is
// that it is built from this fixed list and nothing else.
//
// Only the *first* network is attached at create: Docker's create takes
// exactly one endpoint, and the rest are connected afterwards (ExtraNetworks).
func CreatePayload(spec model.ContainerSpec) map[string]any {
	payload := map[string]any{
		"Image":  spec.Image,
		"Env":    orEmpty(spec.Env),
		"Labels": orEmptyMap(spec.Labels),
	}
	// nil is "the image's own". An empty Entrypoint is a deliberate "none";
	// an empty Cmd beside one is too, as Docker fills Cmd only when there is
	// no Entrypoint.
	if spec.Cmd != nil {
		payload["Cmd"] = spec.Cmd
	}
	if spec.Entrypoint != nil {
		payload["Entrypoint"] = spec.Entrypoint
	}
	if len(spec.Exposed) > 0 {
		exposed := map[string]struct{}{}
		for _, port := range spec.Exposed {
			exposed[port] = struct{}{}
		}
		payload["ExposedPorts"] = exposed
	}
	setIf(payload, "User", spec.User)
	setIf(payload, "WorkingDir", spec.WorkingDir)
	setIf(payload, "StopSignal", spec.StopSignal)
	if spec.StopTimeout != nil {
		payload["StopTimeout"] = *spec.StopTimeout
	}
	if check := spec.Healthcheck; check != nil {
		// A field left out is filled from the image's HEALTHCHECK.
		healthcheck := map[string]any{}
		if len(check.Test) > 0 {
			healthcheck["Test"] = check.Test
		}
		for key, value := range map[string]int64{
			"Interval": int64(check.Interval), "Timeout": int64(check.Timeout),
			"Retries": int64(check.Retries), "StartPeriod": int64(check.StartPeriod),
		} {
			if value != 0 {
				healthcheck[key] = value
			}
		}
		payload["Healthcheck"] = healthcheck
	}

	host := map[string]any{
		"Binds":        binds(spec),
		"Mounts":       longFormMounts(spec),
		"PortBindings": portBindings(spec),
		"Privileged":   spec.Privileged,
	}
	if policy := spec.RestartPolicy; policy != nil {
		host["RestartPolicy"] = map[string]any{"Name": policy.Name, "MaximumRetryCount": policy.MaximumRetryCount}
	}
	if len(spec.CapAdd) > 0 {
		host["CapAdd"] = spec.CapAdd
	}
	if len(spec.CapDrop) > 0 {
		host["CapDrop"] = spec.CapDrop
	}
	setIf(host, "NetworkMode", spec.NetworkMode)
	if len(spec.Devices) > 0 {
		host["Devices"] = spec.Devices
	}
	if len(spec.ExtraHosts) > 0 {
		host["ExtraHosts"] = spec.ExtraHosts
	}
	if len(spec.Sysctls) > 0 {
		host["Sysctls"] = spec.Sysctls
	}
	payload["HostConfig"] = host

	if len(spec.Networks) > 0 {
		primary := spec.Networks[0]
		payload["NetworkingConfig"] = map[string]any{"EndpointsConfig": map[string]any{primary.Name: endpointOf(primary)}}
	}
	return payload
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
// depends on Op: Grace for stop, To for rename, Spec for create, Network for
// connect, Force for remove; Container for all but create.
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
		swap = append(swap, Step{Op: Connect, Container: name, Network: network})
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

func binds(spec model.ContainerSpec) []string {
	binds := []string{}
	for _, mount := range spec.Mounts {
		if mount.Type == model.Bind {
			bind := mount.Source + ":" + mount.Target
			if mount.ReadOnly {
				bind += ":ro"
			}
			binds = append(binds, bind)
		}
	}
	return binds
}

// longFormMounts is the named volumes and tmpfs, which go in Mounts
// long-form; binds stay in Binds.
func longFormMounts(spec model.ContainerSpec) []map[string]any {
	mounts := []map[string]any{}
	for _, mount := range spec.Mounts {
		if mount.Type == model.Bind {
			continue
		}
		long := map[string]any{"Type": mount.Type, "Target": mount.Target, "ReadOnly": mount.ReadOnly}
		if mount.Type == model.Volume {
			long["Source"] = mount.Source
		}
		mounts = append(mounts, long)
	}
	return mounts
}

// portBindings is every binding as it was created, host address included -
// left out only when it was never given, as Docker then binds every
// interface either way. A host port Docker picked is left for it to pick
// again: no HostPort, or the range it was picked from.
func portBindings(spec model.ContainerSpec) map[string][]map[string]string {
	bindings := map[string][]map[string]string{}
	for _, port := range spec.Bindings {
		key := strconv.Itoa(port.ContainerPort) + "/" + string(port.Protocol)
		binding := map[string]string{}
		switch {
		case port.HostRange != "":
			binding["HostPort"] = port.HostRange
		case port.HostPort != 0:
			binding["HostPort"] = strconv.Itoa(port.HostPort)
		}
		if port.HostIP != "" {
			binding["HostIp"] = port.HostIP
		}
		if !slices.ContainsFunc(bindings[key], func(other map[string]string) bool { return maps.Equal(other, binding) }) {
			bindings[key] = append(bindings[key], binding)
		}
	}
	return bindings
}

// endpointOf is the EndpointConfig that attaches one network - the same
// whether in a create's NetworkingConfig or a later connect, so it is written
// once.
func endpointOf(network model.NetworkAttachment) map[string]any {
	endpoint := map[string]any{}
	if len(network.Aliases) > 0 {
		endpoint["Aliases"] = network.Aliases
	}
	ipam := map[string]any{}
	setIf(ipam, "IPv4Address", network.IPv4Address)
	setIf(ipam, "IPv6Address", network.IPv6Address)
	if len(ipam) > 0 {
		endpoint["IPAMConfig"] = ipam
	}
	return endpoint
}

func setIf(into map[string]any, key, value string) {
	if value != "" {
		into[key] = value
	}
}

func orEmptyMap(values map[string]string) map[string]string {
	if values == nil {
		return map[string]string{}
	}
	return maps.Clone(values)
}
