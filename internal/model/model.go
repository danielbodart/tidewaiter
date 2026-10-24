// Package model is the vocabulary the rest of Tidewaiter is written in.
//
// The important distinction, mirroring Portical's Rule-vs-Mapping split, is
// "what is running now" (RunningContainer, read off Docker) versus "what we
// want it pinned to" (the registry's digest for its tag). Every update
// decision is a comparison of the two, so keeping them apart keeps the
// decision honest.
package model

import (
	"fmt"
	"time"
)

// Protocol is a transport the activity gate and the health checks understand:
// TCP and UDP only, as PLAN.md scopes them.
type Protocol string

const (
	TCP Protocol = "tcp"
	UDP Protocol = "udp"
)

// Protocols is every Protocol, in the order they are reported.
var Protocols = []Protocol{TCP, UDP}

// Digest is an image digest, or for a unit a Nix store path.
//
// Opaque outside equality - it is compared, never parsed. `sha256:abc...` for
// a manifest or config digest. Two containers on the same digest are the same
// image however they were tagged, which is the whole basis of "is there
// anything new to pull?". The empty Digest is "none": an image not present
// locally, or a profile that does not exist yet.
type Digest string

// HealthStatus is Docker's health state, plus "none" for a container with no
// HEALTHCHECK.
type HealthStatus string

const (
	Starting  HealthStatus = "starting"
	Healthy   HealthStatus = "healthy"
	Unhealthy HealthStatus = "unhealthy"
	NoHealth  HealthStatus = "none"
)

// PublishedPort is a port the container publishes on the Docker host - or, for
// a systemd unit on the host's network, a port it serves, with HostPort and
// ContainerPort the same.
type PublishedPort struct {
	HostPort      int
	ContainerPort int
	Protocol      Protocol
}

func (p PublishedPort) String() string {
	return fmt.Sprintf("%d:%d/%s", p.HostPort, p.ContainerPort, p.Protocol)
}

// PortBinding is one `-p` as the container was created with it, host address
// and all. A recreate replays every one: dropping the address of a
// `-p 127.0.0.1:5432:5432` would bring a loopback-only database back up on
// every interface. An empty HostIP is Docker's default, every interface.
//
// A HostPort of 0 is a binding that left the host port to Docker: a bare
// `-p 80` (or `-p 127.0.0.1::80`), which Docker gives an ephemeral port each
// time the container starts, or `-p 8000-8010:80`, which it gives one from
// HostRange. Replayed as asked, so a recreate gets Docker's pick again - as a
// compose recreate does - rather than pinning the port the old one happened
// to get, or not publishing the container port at all.
type PortBinding struct {
	HostIP string
	// The host port range Docker picks from, as given (`8000-8010`), for the
	// one form Docker keeps a range in: a host range onto a single container
	// port. A range onto a range (`-p 8000-8001:80-81`) is stored by Docker as
	// one binding per port, so never reaches here as a range.
	HostRange string
	PublishedPort
}

// Published is the ports published on the host, once each, whatever
// addresses they are bound on: what activity and health look at. Docker
// shows a plain `-p 8080:80` bound on 0.0.0.0 and on :: as two bindings, and
// a port counts once however many addresses it listens on.
//
// A binding whose host port Docker picks (HostPort 0) is left out: which port
// it got is not in the spec, and a detector or probe given port 0 would watch
// or dial nothing while reading as though it had looked.
func (spec ContainerSpec) Published() []PublishedPort {
	found := []PublishedPort{}
	seen := map[string]bool{}
	for _, binding := range spec.Bindings {
		if binding.HostPort == 0 {
			continue
		}
		key := fmt.Sprintf("%s/%d", binding.Protocol, binding.HostPort)
		if !seen[key] {
			seen[key] = true
			found = append(found, binding.PublishedPort)
		}
	}
	return found
}

// ContainerSpec is everything about a container that a faithful recreate must
// reproduce.
//
// Read off `docker inspect` and re-applied verbatim on recreate, minus the
// image. Deliberately a flat, hand-picked set of fields rather than the whole
// inspect blob: the blob also carries runtime-only state (the container's id,
// its current IP, its PID) that must never be sent back to /containers/create.
// The Docker side owns the mapping in both directions; this is the shape in
// the middle.
//
// A nil slice, a nil pointer or an empty string is "not set", which recreate
// must leave to the image rather than send as an explicit empty value - an
// empty Entrypoint is not the same as the image's own.
//
// Read off inspect, the config fields (Env, Cmd, Entrypoint, Labels, Exposed,
// User, WorkingDir, StopSignal, Healthcheck) are what Docker made of the
// container's own settings merged with its image's defaults; a swap takes
// the image's defaults back out (WithoutImageDefaults) so the new image's
// take their place.
type ContainerSpec struct {
	Name string
	// The image ref as configured, e.g. "nginx:1.27" - the tag we keep chasing.
	Image         string
	Env           []string
	Cmd           []string
	Entrypoint    []string
	Labels        map[string]string
	Mounts        []Mount
	Bindings      []PortBinding
	Exposed       []string
	Networks      []NetworkAttachment
	NetworkMode   string
	RestartPolicy *RestartPolicy
	CapAdd        []string
	CapDrop       []string
	Privileged    bool
	Devices       []Device
	ExtraHosts    []string
	Sysctls       map[string]string
	User          string
	WorkingDir    string
	StopSignal    string
	// The container's own stop timeout, in seconds, if it set one.
	StopTimeout *int
	Healthcheck *Healthcheck
}

// MountType is Docker's long-form mount type.
type MountType string

const (
	Bind   MountType = "bind"
	Volume MountType = "volume"
	Tmpfs  MountType = "tmpfs"
)

// Mount is faithful to Docker's long form, so bind, volume and tmpfs mounts
// survive a recreate.
type Mount struct {
	Type     MountType
	Source   string
	Target   string
	ReadOnly bool
	// A volume with no name - recreate cannot reproduce it faithfully.
	Anonymous bool
}

// NetworkAttachment is a network the container is attached to, with anything
// needed to reattach it.
type NetworkAttachment struct {
	Name    string
	Aliases []string
	// A static address, when one was assigned rather than handed out by IPAM.
	IPv4Address string
	IPv6Address string
	// The address the container is ACTUALLY reachable at right now, as Docker
	// reports it under `NetworkSettings.Networks[name].IPAddress` - the
	// runtime IP IPAM handed out, distinct from the static IPv4Address a user
	// pinned.
	//
	// Runtime-only, so recreate must NOT send it back to /containers/create
	// (like the PID, it is live state). It exists for one reader: the
	// port-connect health check, which connects to IPAddress:ContainerPort to
	// test the exact path a client takes on a bridge or user network -
	// bypassing docker-proxy, which would falsely accept, and the netns bind
	// check, which is blind to *which* address a socket listens on.
	IPAddress string
}

type RestartPolicy struct {
	Name              string
	MaximumRetryCount int
}

type Device struct {
	PathOnHost        string
	PathInContainer   string
	CgroupPermissions string
}

// Healthcheck is an image or container HEALTHCHECK. An empty Test or a zero
// duration or retry count is "not set", as the Engine API reads it: Docker
// fills each such field from the image's own HEALTHCHECK.
type Healthcheck struct {
	Test        []string
	Interval    time.Duration
	Timeout     time.Duration
	Retries     int
	StartPeriod time.Duration
}

// Unset is whether the healthcheck sets nothing at all, leaving every field
// to the image.
func (check Healthcheck) Unset() bool {
	return len(check.Test) == 0 && check.Interval == 0 && check.Timeout == 0 && check.Retries == 0 && check.StartPeriod == 0
}

// RunningContainer is a container as it is running now, with the facts an
// update decision needs.
//
// PID is the container's main process as seen from the host - Tidewaiter runs
// in the host PID namespace, so it is directly usable to read
// /proc/<pid>/net/* for the netns-scoped detectors and health checks.
type RunningContainer struct {
	ID   string
	PID  int
	Spec ContainerSpec
	// The image config ID this container was created from, as inspect reports
	// it under `.Image` (a local content-address of the image *config*).
	//
	// NOT the registry manifest digest, and deliberately NOT used to decide
	// whether an update is due - those two are different hashes and would
	// never compare equal. The update decision compares the registry's
	// manifest digest for the tag against the *local image's* RepoDigest; this
	// field is kept only for logging and diagnostics.
	ImageID string
	Health  HealthStatus
	// Whether the container is genuinely up right now: `State.Running` true
	// AND `State.Restarting` false. A container that has exited or is
	// crash-looping under a restart policy is NOT running by this measure -
	// both mean the new image failed to stay up, which the uptime health check
	// reads as an active failure.
	Running bool
}

// DesiredImage is what the registry says this container's tag should resolve
// to, this pass.
type DesiredImage struct {
	Container string
	Ref       string
	Digest    Digest
}
