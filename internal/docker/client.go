// Package docker is Docker containers as a source of updates: the Engine API
// client, the labels that opt a container in, the faithful recreate a swap
// is made of, the `docker` health check, and the Source that ties them to the
// daemon.
package docker

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danielbodart/gokit/httpx"
	"github.com/danielbodart/tidewaiter/internal/model"
)

// API is the Engine API version spoken. 1.41 ships with Docker 20.10, old
// enough to be safe.
const API = "v1.41"

// RequestTimeout bounds every Engine API call that is meant to come straight
// back.
//
// Set per request rather than as the http.Client's Timeout, which would also
// cut off /events: that is a long poll that sits idle for as long as nothing
// happens, and must be allowed to. A pass runs on a context that is never
// cancelled - the daemon finishes a swap rather than abandon it - so these
// bounds are what stop a wedged Docker from holding a pass for ever.
const RequestTimeout = 30 * time.Second

// PullTimeout bounds a pull, which streams progress for as long as the
// download takes: generous, as a large image on a slow line is minutes, but
// finite, so a pull that wedges cannot hold a prefetch slot for good.
const PullTimeout = 30 * time.Minute

// Event is a Docker lifecycle event, narrowed to what Tidewaiter watches.
//
// Status carries the verdict of a `health_status` event ("health_status:
// healthy" has Status "healthy"), which is the one event Tidewaiter acts on -
// during a swap's health gate, when the container defines a HEALTHCHECK.
type Event struct {
	Action    string
	Container string
	Status    string
}

// Client talks to the Docker Engine API.
//
// It takes an http.RoundTripper rather than a socket path, so the tests hand
// it an in-memory Docker and it cannot tell the difference. Every call reads
// its response body to the end and closes it: an unread reply on the
// kept-alive socket leaves the *next* request reading a stale one.
type Client struct {
	http *http.Client
}

// NewClient is a client for the Engine API reached through transport -
// httpx.UnixSocket for a real Docker.
func NewClient(transport http.RoundTripper) *Client {
	return &Client{http: &http.Client{Transport: transport}}
}

// Containers is every container carrying label, fully inspected.
func (c *Client) Containers(ctx context.Context, label string) ([]model.RunningContainer, error) {
	var listed []struct {
		ID string `json:"Id"`
	}
	query := url.Values{"filters": {filters(map[string][]string{"label": {label}})}}
	if err := c.quick(ctx, http.MethodGet, "/containers/json", query, nil, &listed); err != nil {
		return nil, err
	}
	// Inspected one at a time rather than mapped off the list: the list
	// carries neither the PID nor the full config a faithful recreate needs,
	// so an inspect per opted-in container is unavoidable. There are only
	// ever a handful.
	containers := make([]model.RunningContainer, 0, len(listed))
	for _, entry := range listed {
		container, err := c.Inspect(ctx, entry.ID)
		if err != nil {
			return nil, err
		}
		containers = append(containers, container)
	}
	return containers, nil
}

// Inspect is one container, fully inspected - by name, id or short id.
func (c *Client) Inspect(ctx context.Context, container string) (model.RunningContainer, error) {
	var raw Inspected
	if err := c.quick(ctx, http.MethodGet, "/containers/"+url.PathEscape(container)+"/json", nil, nil, &raw); err != nil {
		return model.RunningContainer{}, err
	}
	return raw.Running(), nil
}

// LocalImage is a local image as /images/{name}/json shows it: the two things
// Tidewaiter reads off one.
type LocalImage struct {
	// The manifest digest Docker records in RepoDigests, or "" for an image
	// built rather than pulled, which has no registry digest.
	Digest model.Digest
	// What the image's own config sets, which Docker fills into a container
	// created from it wherever the container did not set its own.
	Defaults ImageDefaults
}

// Image is a local image, by id or ref; found is false when it is not here.
//
// One lookup gives both the digest and the defaults, so the swap's need for
// the running image's defaults costs no call beyond the digest read every
// pass already makes.
func (c *Client) Image(ctx context.Context, image string) (local LocalImage, found bool, err error) {
	var raw struct {
		RepoDigests []string
		Config      InspectedConfig
	}
	found, err = c.lookup(ctx, "/images/"+url.PathEscape(image)+"/json", &raw)
	if err != nil || !found {
		return LocalImage{}, found, err
	}
	local.Defaults = raw.Config.defaults()
	if len(raw.RepoDigests) > 0 {
		// `nginx@sha256:...`: the part after the `@` is what the registry
		// returns too, so the two compare.
		first := raw.RepoDigests[0]
		local.Digest = model.Digest(first[strings.LastIndex(first, "@")+1:])
	}
	return local, true, nil
}

// ImageDigest is the manifest digest of a local image, by id or ref, as
// Docker records it in RepoDigests - or "" when that image is not here, or
// was built rather than pulled and so has no registry digest.
func (c *Client) ImageDigest(ctx context.Context, image string) (model.Digest, error) {
	local, _, err := c.Image(ctx, image)
	return local.Digest, err
}

// NetworkDriver is a network's driver (bridge, macvlan, overlay, host, ...),
// by name or id, or "" when the network is gone.
//
// A missing network is a normal answer (removed under us), not an error: the
// port-connect probe reads "" as not reachable, so an IP it cannot classify
// is never probed.
func (c *Client) NetworkDriver(ctx context.Context, network string) (string, error) {
	var raw struct{ Driver string }
	_, err := c.lookup(ctx, "/networks/"+url.PathEscape(network), &raw)
	return raw.Driver, err
}

// Pull an image. auth is a ready X-Registry-Auth header value, or "".
//
// The response streams progress until the pull finishes, and is read to the
// end: that is what makes the call last until the image is actually here.
// Docker reports a pull that fails part-way inside that stream, with the 200
// already sent, so the stream is read for it rather than trusting the status.
func (c *Client) Pull(ctx context.Context, ref, auth string) error {
	ctx, cancel := context.WithTimeout(ctx, PullTimeout)
	defer cancel()
	fail := func(err error) error { return fmt.Errorf("Docker API pull %s failed: %w", ref, err) }
	response, err := c.do(ctx, http.MethodPost, "/images/create", url.Values{"fromImage": {tagged(ref)}}, nil, func(request *http.Request) {
		if auth != "" {
			request.Header.Set("X-Registry-Auth", auth)
		}
	})
	if err != nil {
		return fail(err)
	}
	defer drain(response)
	decoder := json.NewDecoder(response.Body)
	for {
		var progress struct {
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := decoder.Decode(&progress); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fail(err)
		}
		if message := cmp.Or(progress.ErrorDetail.Message, progress.Error); message != "" {
			return fail(errors.New(message))
		}
	}
}

// tagged is ref with `:latest` added when it names neither a tag nor a digest.
//
// Given a bare name the Engine API pulls every tag of the repository, not the
// one the registry lookup compared against (registry.ParseRef reads a bare
// name as latest); the docker CLI only avoids that by adding the tag itself.
// A colon before the last slash is a registry's port, not a tag.
func tagged(ref string) string {
	if strings.Contains(ref, "@") || strings.Contains(ref[strings.LastIndex(ref, "/")+1:], ":") {
		return ref
	}
	return ref + ":latest"
}

// Create a container from a spec, returning its new id.
func (c *Client) Create(ctx context.Context, spec model.ContainerSpec) (string, error) {
	var created struct {
		ID string `json:"Id"`
	}
	err := c.quick(ctx, http.MethodPost, "/containers/create", url.Values{"name": {spec.Name}}, CreatePayload(spec), &created)
	return created.ID, err
}

func (c *Client) Start(ctx context.Context, container string) error {
	return c.quick(ctx, http.MethodPost, "/containers/"+url.PathEscape(container)+"/start", nil, nil, nil)
}

// Stop a container, giving it graceSeconds to drain before SIGKILL.
func (c *Client) Stop(ctx context.Context, container string, graceSeconds int) error {
	graceSeconds = max(0, graceSeconds)
	// Docker answers once the container has stopped, which may be the whole
	// grace later: the bound is the grace plus the usual one.
	return c.bounded(ctx, time.Duration(graceSeconds)*time.Second+RequestTimeout, http.MethodPost, "/containers/"+url.PathEscape(container)+"/stop", url.Values{"t": {strconv.Itoa(graceSeconds)}}, nil, nil)
}

// Remove a container and its anonymous volumes; force kills it first if it
// is running.
func (c *Client) Remove(ctx context.Context, container string, force bool) error {
	query := url.Values{"v": {"1"}}
	if force {
		query.Set("force", "1")
	}
	return c.quick(ctx, http.MethodDelete, "/containers/"+url.PathEscape(container), query, nil, nil)
}

func (c *Client) Rename(ctx context.Context, container, name string) error {
	return c.quick(ctx, http.MethodPost, "/containers/"+url.PathEscape(container)+"/rename", url.Values{"name": {name}}, nil, nil)
}

// RemoveImage removes a local image, or one repository digest of it.
func (c *Client) RemoveImage(ctx context.Context, image string) error {
	return c.quick(ctx, http.MethodDelete, "/images/"+url.PathEscape(image), nil, nil, nil)
}

// ConnectNetwork attaches an already-created container to another network.
func (c *Client) ConnectNetwork(ctx context.Context, container string, network model.NetworkAttachment) error {
	body := map[string]any{"Container": container, "EndpointConfig": endpointOf(network)}
	return c.quick(ctx, http.MethodPost, "/networks/"+url.PathEscape(network.Name)+"/connect", nil, body, nil)
}

// NetBytes is a container's cumulative network bytes, received plus sent,
// summed over its interfaces.
//
// A single point-in-time counter, deliberately: a rate needs two of these an
// interval apart, which the netio detector works out across passes.
func (c *Client) NetBytes(ctx context.Context, container string) (uint64, error) {
	// stream=false is one snapshot rather than a feed that never ends;
	// one-shot skips the second sample Docker otherwise waits a second for,
	// which only the CPU figures need.
	var raw Stats
	query := url.Values{"stream": {"false"}, "one-shot": {"true"}}
	if err := c.quick(ctx, http.MethodGet, "/containers/"+url.PathEscape(container)+"/stats", query, nil, &raw); err != nil {
		return 0, err
	}
	return raw.NetBytes(), nil
}

// Stats is the slice of /stats read: each interface's byte counters.
type Stats struct {
	Networks map[string]struct {
		RxBytes uint64 `json:"rx_bytes"`
		TxBytes uint64 `json:"tx_bytes"`
	} `json:"networks"`
}

// NetBytes totals rx and tx over every interface. A container with no
// networks block (host networking, or no traffic yet) totals zero, which a
// rate reads as "nothing moved" - correct.
func (s Stats) NetBytes() uint64 {
	var total uint64
	for _, iface := range s.Networks {
		total += iface.RxBytes + iface.TxBytes
	}
	return total
}

// Events calls notify for each start, die, stop, kill and health_status of a
// container carrying label, until notify says stop, ctx is done, or the
// stream drops.
//
// Returns nil when notify stopped it, ctx is done or Docker closed the
// stream; reopening a dropped stream is the caller's business.
func (c *Client) Events(ctx context.Context, label string, notify func(Event) (stop bool)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	query := url.Values{"filters": {filters(map[string][]string{
		"type":  {"container"},
		"event": {"start", "die", "stop", "kill", "health_status"},
		"label": {label},
	})}}
	response, err := c.do(ctx, http.MethodGet, "/events", query, nil, nil)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	// Closing the body is what ends the stream when notify stops it; the
	// cancel above ends it when the caller's ctx does.
	defer response.Body.Close()

	// One JSON object per line, as each event happens: the scanner hands
	// each over as soon as its line is complete.
	lines := bufio.NewScanner(response.Body)
	for lines.Scan() {
		line := bytes.TrimSpace(lines.Bytes())
		if len(line) == 0 {
			continue
		}
		var raw struct {
			Action string
			ID     string `json:"id"`
			Actor  struct {
				ID         string
				Attributes map[string]string
			}
		}
		if err := json.Unmarshal(line, &raw); err != nil {
			return fmt.Errorf("Docker API /%s/events failed: %w", API, err)
		}
		event := Event{Action: raw.Action, Container: cmp.Or(raw.Actor.Attributes["name"], raw.ID, raw.Actor.ID)}
		// "health_status: healthy": the verdict split out, so a consumer
		// matches on it without string surgery.
		if verdict, found := strings.CutPrefix(raw.Action, "health_status"); found {
			event.Status = strings.TrimSpace(strings.TrimPrefix(verdict, ":"))
		}
		if notify(event) {
			return nil
		}
	}
	if err := lines.Err(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("Docker API /%s/events failed: %w", API, err)
	}
	return nil
}

// do makes one request and checks its status. ctx alone bounds it, so the
// caller decides whether it is the kind that comes straight back.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any, prepare func(*http.Request)) (*http.Response, error) {
	target := "/" + API + path
	fail := func(err error) error { return fmt.Errorf("Docker API %s failed: %w", target, err) }

	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fail(err)
		}
		reader = bytes.NewReader(data)
	}
	// The path's parts are escaped by their callers, so an image ref's slashes
	// reach Docker as one segment. The host is a placeholder: the transport
	// decides where this goes.
	unescaped, err := url.PathUnescape(target)
	if err != nil {
		return nil, fail(err)
	}
	address := &url.URL{Scheme: "http", Host: "docker", Path: unescaped, RawPath: target, RawQuery: query.Encode()}
	request, err := http.NewRequestWithContext(ctx, method, address.String(), reader)
	if err != nil {
		return nil, fail(err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if prepare != nil {
		prepare(request)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, fail(err)
	}
	if err := httpx.Check(response); err != nil {
		return nil, fail(err)
	}
	return response, nil
}

// quick is a call bounded by RequestTimeout, whose body is always read to
// the end and closed. A nil into discards the reply.
func (c *Client) quick(ctx context.Context, method, path string, query url.Values, body, into any) error {
	return c.bounded(ctx, RequestTimeout, method, path, query, body, into)
}

func (c *Client) bounded(ctx context.Context, timeout time.Duration, method, path string, query url.Values, body, into any) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	response, err := c.do(ctx, method, path, query, body, nil)
	if err != nil {
		return err
	}
	defer drain(response)
	if into == nil {
		return nil
	}
	if err := json.NewDecoder(response.Body).Decode(into); err != nil {
		return fmt.Errorf("Docker API /%s%s failed: %w", API, path, err)
	}
	return nil
}

// lookup is a GET where a 404 is an answer - "not here" - rather than an
// error; found says which.
func (c *Client) lookup(ctx context.Context, path string, into any) (found bool, err error) {
	err = c.quick(ctx, http.MethodGet, path, nil, nil, into)
	if status := (*httpx.StatusError)(nil); errors.As(err, &status) && status.StatusCode == http.StatusNotFound {
		return false, nil
	}
	return err == nil, err
}

func drain(response *http.Response) {
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
}

// filters is how Docker wants its filters: a JSON map of name to values.
func filters(spec map[string][]string) string {
	data, _ := json.Marshal(spec)
	return string(data)
}

// Inspected is the shape of `docker inspect <container>` read.
type Inspected struct {
	ID    string `json:"Id"`
	Name  string
	Image string
	State struct {
		Pid    int
		Health *struct {
			Status string
		}
		Running    bool
		Restarting bool
	}
	Config     InspectedConfig
	HostConfig struct {
		// Long-form, as created: a volume's Source is its name.
		Mounts []struct {
			Type, Source, Target string
			ReadOnly             bool
		}
		PortBindings  map[string][]struct{ HostIP, HostPort string }
		RestartPolicy *struct {
			Name              string
			MaximumRetryCount int
		}
		CapAdd      []string
		CapDrop     []string
		Privileged  bool
		NetworkMode string
		Devices     []model.Device
		ExtraHosts  []string
		Sysctls     map[string]string
	}
	// As mounted: binds, volumes and tmpfs alike, a volume with its Name.
	Mounts          []InspectedMount
	NetworkSettings struct {
		Networks map[string]InspectedNetwork
	}
}

// InspectedConfig is the Config of a container's inspect, or of an image's:
// Docker gives both the same shape.
//
// The slices keep JSON's null apart from []: an Entrypoint of [] is a
// container that asked for none, where null is one that left it to its image.
type InspectedConfig struct {
	Image        string
	Env          []string
	Cmd          []string
	Entrypoint   []string
	Labels       map[string]string
	ExposedPorts map[string]struct{}
	User         string
	WorkingDir   string
	StopSignal   string
	StopTimeout  *int
	Healthcheck  *struct {
		Test        []string
		Interval    time.Duration
		Timeout     time.Duration
		Retries     int
		StartPeriod time.Duration
	}
}

// healthcheck is the HEALTHCHECK the config sets, or nil when it sets none.
//
// Any field set counts, not only a Test: a container given just
// `--health-interval` is stored with that alone when its image has no
// HEALTHCHECK to fill the rest from, and a recreate must keep it.
func (config InspectedConfig) healthcheck() *model.Healthcheck {
	check := config.Healthcheck
	if check == nil {
		return nil
	}
	found := &model.Healthcheck{
		Test: check.Test, Interval: check.Interval, Timeout: check.Timeout,
		Retries: check.Retries, StartPeriod: check.StartPeriod,
	}
	if found.Unset() {
		return nil
	}
	if len(found.Test) == 0 {
		found.Test = nil
	}
	return found
}

// defaults is an image's config as the defaults it gives a container.
func (config InspectedConfig) defaults() ImageDefaults {
	return ImageDefaults{
		Env:         config.Env,
		Cmd:         config.Cmd,
		Entrypoint:  config.Entrypoint,
		Labels:      config.Labels,
		Exposed:     slices.Sorted(maps.Keys(config.ExposedPorts)),
		User:        config.User,
		WorkingDir:  config.WorkingDir,
		StopSignal:  config.StopSignal,
		Healthcheck: config.healthcheck(),
	}
}

type InspectedMount struct {
	Type        string
	Name        string
	Source      string
	Destination string
	RW          bool
}

type InspectedNetwork struct {
	Aliases    []string
	IPAMConfig *struct {
		IPv4Address string
		IPv6Address string
	}
	// The runtime address, distinct from IPAMConfig's static one - what the
	// container is reachable at on this network right now.
	IPAddress string
}

// Running is the container as Tidewaiter sees it.
//
// This mapping is the fiddliest read Tidewaiter does, and a field wrong here
// silently breaks a recreate later, so it is tested against captured inspect
// JSON.
func (raw Inspected) Running() model.RunningContainer {
	health := model.NoHealth
	if raw.State.Health != nil {
		switch status := model.HealthStatus(raw.State.Health.Status); status {
		case model.Starting, model.Healthy, model.Unhealthy:
			health = status
		}
	}
	return model.RunningContainer{
		ID:  raw.ID,
		PID: raw.State.Pid,
		// The config id, for diagnostics and for looking up the manifest
		// digest of the very image this container runs - never compared with
		// a registry digest itself.
		ImageID: raw.Image,
		Health:  health,
		// Genuinely up: running and NOT mid-restart. An exited container and a
		// crash-looping one are both "not staying up", which uptime fails.
		Running: raw.State.Running && !raw.State.Restarting,
		Spec:    raw.Spec(),
	}
}

// Spec is the faithful-recreate spec in an inspect.
//
// Only the fields the recreate knows how to re-apply are read; runtime-only
// ones (the id, the state, the live IP) are left behind so they can never
// leak back into a create.
func (raw Inspected) Spec() model.ContainerSpec {
	config, host := raw.Config, raw.HostConfig
	spec := model.ContainerSpec{
		Name:        strings.TrimPrefix(cmp.Or(raw.Name, raw.ID), "/"),
		Image:       config.Image,
		Env:         orEmpty(config.Env),
		Cmd:         config.Cmd,
		Entrypoint:  config.Entrypoint,
		Labels:      config.Labels,
		Mounts:      raw.mounts(),
		Bindings:    portBindingsOf(host.PortBindings),
		Exposed:     orEmpty(slices.Sorted(maps.Keys(config.ExposedPorts))),
		Networks:    networksOf(raw.NetworkSettings.Networks, host.NetworkMode),
		NetworkMode: host.NetworkMode,
		CapAdd:      orEmpty(host.CapAdd),
		CapDrop:     orEmpty(host.CapDrop),
		Privileged:  host.Privileged,
		Devices:     orEmpty(host.Devices),
		ExtraHosts:  orEmpty(host.ExtraHosts),
		Sysctls:     host.Sysctls,
		User:        config.User,
		WorkingDir:  config.WorkingDir,
		StopSignal:  config.StopSignal,
		StopTimeout: config.StopTimeout,
	}
	if spec.Labels == nil {
		spec.Labels = map[string]string{}
	}
	if spec.Sysctls == nil {
		spec.Sysctls = map[string]string{}
	}
	if policy := host.RestartPolicy; policy != nil && policy.Name != "" && policy.Name != "no" {
		spec.RestartPolicy = &model.RestartPolicy{Name: policy.Name, MaximumRetryCount: policy.MaximumRetryCount}
	}
	spec.Healthcheck = config.healthcheck()
	return spec
}

// A named volume has a Name; an anonymous one is a 64-hex Docker id used as
// both Name and Source.
var anonymousVolume = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (raw Inspected) mounts() []model.Mount {
	mounts := []model.Mount{}
	add := func(kind, name, source, target string, readOnly bool) {
		switch model.MountType(kind) {
		case model.Bind, model.Volume, model.Tmpfs:
		default:
			return
		}
		mount := model.Mount{Type: model.MountType(kind), Source: cmp.Or(source, name), Target: target, ReadOnly: readOnly}
		if mount.Type == model.Volume {
			// Reattached by its Name, never the host _data path Docker also
			// reports: recreating from the path would make a fresh bind, not
			// reuse the volume. The recreate refuses an anonymous one rather
			// than mint a fresh empty volume and silently lose the data.
			mount.Source = cmp.Or(name, source)
			mount.Anonymous = anonymousVolume.MatchString(name)
		}
		mounts = append(mounts, mount)
	}
	if raw.Mounts != nil {
		for _, mount := range raw.Mounts {
			add(mount.Type, mount.Name, mount.Source, mount.Destination, !mount.RW)
		}
		return mounts
	}
	// Only an inspect with no Mounts at all falls back to what was asked for
	// at create, where a volume's Source is its name.
	for _, mount := range raw.HostConfig.Mounts {
		name := ""
		if mount.Type == string(model.Volume) {
			name = mount.Source
		}
		add(mount.Type, name, mount.Source, mount.Target, mount.ReadOnly)
	}
	return mounts
}

// portBindingsOf is every host binding the container was created with, each
// with the address it is bound on, so a recreate puts it back exactly; the
// spec's Published() is the once-per-port view of them.
//
// Docker stores a host port as text, in one of three forms: a port (`8080`);
// nothing, for a `-p 80` that leaves the port to Docker; or a range
// (`8000-8010`), for a host range onto a single container port, which Docker
// also picks from. The last two are HostPort 0, the range kept to replay. A
// range onto a range (`-p 8000-8001:80-81`) Docker splits into one binding per
// port before storing it, so it needs nothing of its own here.
func portBindingsOf(bindings map[string][]struct{ HostIP, HostPort string }) []model.PortBinding {
	found := []model.PortBinding{}
	// In container-port order, so what is said about them is stable.
	for _, portProtocol := range slices.Sorted(maps.Keys(bindings)) {
		portText, protocol, _ := strings.Cut(portProtocol, "/")
		containerPort, err := strconv.Atoi(portText)
		if err != nil || !slices.Contains(model.Protocols, model.Protocol(protocol)) {
			continue
		}
		for _, binding := range bindings[portProtocol] {
			found = append(found, model.PortBinding{
				HostIP:        binding.HostIP,
				PublishedPort: model.PublishedPort{ContainerPort: containerPort, Protocol: model.Protocol(protocol)},
			})
			last := &found[len(found)-1]
			switch {
			case binding.HostPort == "":
			case hostRange.MatchString(binding.HostPort):
				last.HostRange = binding.HostPort
			default:
				hostPort, err := strconv.Atoi(binding.HostPort)
				if err != nil || hostPort <= 0 {
					// Nothing Docker writes; replaying a guess at it could
					// publish the wrong port, so it is left out.
					found = found[:len(found)-1]
					continue
				}
				last.HostPort = hostPort
			}
		}
	}
	return found
}

var hostRange = regexp.MustCompile(`^[0-9]+-[0-9]+$`)

// networksOf is the container's networks, the one its NetworkMode names
// first and the rest by name.
//
// The order is load-bearing: a create attaches only the first network, and
// the rest are connected afterwards. Docker writes the map in name order,
// which says nothing about which network is the container's own; NetworkMode
// does.
func networksOf(networks map[string]InspectedNetwork, mode string) []model.NetworkAttachment {
	names := slices.Sorted(maps.Keys(networks))
	if index := slices.Index(names, mode); index > 0 {
		names = slices.Concat([]string{mode}, slices.Delete(names, index, index+1))
	}
	attachments := []model.NetworkAttachment{}
	for _, name := range names {
		network := networks[name]
		attachment := model.NetworkAttachment{Name: name, Aliases: orEmpty(network.Aliases), IPAddress: network.IPAddress}
		if network.IPAMConfig != nil {
			attachment.IPv4Address = network.IPAMConfig.IPv4Address
			attachment.IPv6Address = network.IPAMConfig.IPv6Address
		}
		attachments = append(attachments, attachment)
	}
	return attachments
}

func orEmpty[T any](values []T) []T {
	if values == nil {
		return []T{}
	}
	return values
}
