package docker

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/danielbodart/gokit/httpx"
	"github.com/danielbodart/tidewaiter/internal/model"
	"github.com/danielbodart/tidewaiter/internal/registry"
)

// fakeDocker is the Engine API as an http.Handler, so a whole update flow
// runs against the real client with no daemon and no socket.
//
// Tests arrange it through its fields (under mu, before anything runs, or by
// method once something might be) and read back what was asked of it.
// Lifecycle calls change what is running, so a later inspect sees the swap
// that happened. The event stream is served as Docker serves it - a line per
// event, on a response that stays open - through
// httpx.StreamingHandlerTransport.
type fakeDocker struct {
	mu      sync.Mutex
	running []model.RunningContainer
	// The manifest digest each LOCAL image has, by image id AND by tag ref -
	// Docker's /images/{name}/json takes both. A tag's entry moves when a pull
	// lands a new image; an id's never does, which is why "current" is looked
	// up by the container's image id rather than its tag.
	images map[string]model.Digest
	// What each image's own config sets, by its manifest digest: served by
	// /images/{name}/json, and merged into a container created from it as
	// Docker merges it. Absent is an image that sets nothing.
	configs map[model.Digest]ImageDefaults
	// What a pull of a ref lands: a fresh local image (a new config id) whose
	// RepoDigest is this, with the tag repointed at it - Docker fetching a
	// newer manifest while the old container keeps its old image id.
	pullLands map[string]model.Digest
	// Non-empty makes listing and inspecting fail, as a Docker restart would.
	failWith string
	// Non-empty makes an image lookup fail, as a Docker that cannot answer.
	failImage string
	// Non-empty makes a pull fail part-way, inside its 200 progress stream,
	// as Docker reports a pull that cannot finish.
	failPull string
	// Makes one lifecycle call fail, to exercise a swap failing at that step.
	failOp map[Op]string
	// How inspect reports a container's health and run state, by name, once
	// it is (re)created; otherwise the container's own.
	healthByName  map[string]model.HealthStatus
	runningByName map[string]bool
	// A network's driver by name; absent is bridge, "" is a network that is
	// gone (404).
	drivers map[string]string
	// Cumulative network bytes per container id, for netio.
	netBytes     map[string]uint64
	netBytesFail string

	pulls []pull
	// Each create as Docker shows the container after: the spec asked for,
	// merged with its image's defaults.
	creates       []model.ContainerSpec
	payloads      []map[string]any
	starts        []string
	stops         []stopped
	removes       []removed
	renames       []renamed
	removedImages []string
	connects      []connected

	// Events not yet taken by a stream. Kept while no stream is open, so a
	// stream reopened after a drop sees what happened in between.
	queue   []Event
	wake    chan struct{}
	streams map[int]chan struct{}
	opened  int
	// What each container shows on inspect beyond its typed spec, by id.
	extras map[string]inspectExtra
	// The Engine API versions this Docker speaks, in front of mux.
	api *versioned
	mux *http.ServeMux
}

type pull struct{ ref, auth string }
type stopped struct {
	container string
	grace     int
}
type removed struct {
	container string
	force     bool
}
type renamed struct{ container, to string }
type connected struct {
	container string
	network   model.NetworkAttachment
	// The endpoint settings as sent.
	endpoint map[string]any
}

func newFakeDocker() *fakeDocker {
	f := &fakeDocker{
		images: map[string]model.Digest{}, configs: map[model.Digest]ImageDefaults{}, pullLands: map[string]model.Digest{}, failOp: map[Op]string{},
		healthByName: map[string]model.HealthStatus{}, runningByName: map[string]bool{},
		drivers: map[string]string{}, netBytes: map[string]uint64{},
		wake: make(chan struct{}, 1), streams: map[int]chan struct{}{}, extras: map[string]inspectExtra{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{version}/containers/json", f.list)
	mux.HandleFunc("GET /{version}/containers/{id}/json", f.inspect)
	mux.HandleFunc("GET /{version}/containers/{id}/stats", f.stats)
	mux.HandleFunc("GET /{version}/images/{name}/json", f.image)
	mux.HandleFunc("GET /{version}/networks/{name}", f.network)
	mux.HandleFunc("GET /{version}/events", f.events)
	mux.HandleFunc("POST /{version}/images/create", f.pull)
	mux.HandleFunc("POST /{version}/containers/create", f.create)
	mux.HandleFunc("POST /{version}/containers/{id}/start", f.start)
	mux.HandleFunc("POST /{version}/containers/{id}/stop", f.stop)
	mux.HandleFunc("POST /{version}/containers/{id}/rename", f.rename)
	mux.HandleFunc("DELETE /{version}/containers/{id}", f.remove)
	mux.HandleFunc("DELETE /{version}/images/{name}", f.removeImage)
	mux.HandleFunc("POST /{version}/networks/{name}/connect", f.connect)
	f.mux = mux
	f.api = &versioned{oldest: "1.24", newest: NewestAPI, next: mux}
	return f
}

func (f *fakeDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) { f.api.ServeHTTP(w, r) }

// client is a Client talking to this Docker.
func (f *fakeDocker) client() *Client {
	return NewClient(httpx.StreamingHandlerTransport(f), nil)
}

// add runs containers, without saying so on the event stream.
func (f *fakeDocker) add(containers ...model.RunningContainer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running = append(f.running, containers...)
}

// with changes the fake under its lock, for a test arranging it while the
// source may be using it.
func (f *fakeDocker) with(change func(f *fakeDocker)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

// read reads the fake under its lock.
func read[T any](f *fakeDocker, get func(f *fakeDocker) T) T {
	f.mu.Lock()
	defer f.mu.Unlock()
	return get(f)
}

func (f *fakeDocker) emit(event Event) {
	f.mu.Lock()
	f.queue = append(f.queue, event)
	f.mu.Unlock()
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

// dropStreams ends every open event stream, as Docker restarting does.
func (f *fakeDocker) dropStreams() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, drop := range f.streams {
		close(drop)
		delete(f.streams, id)
	}
}

func (f *fakeDocker) open() int       { return read(f, func(f *fakeDocker) int { return len(f.streams) }) }
func (f *fakeDocker) everOpened() int { return read(f, func(f *fakeDocker) int { return f.opened }) }

func answer(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}

func refuse(w http.ResponseWriter, status int, message string) {
	answer(w, status, map[string]string{"message": message})
}

// find is a running container by name or id; the caller holds mu.
func (f *fakeDocker) find(container string) int {
	return slices.IndexFunc(f.running, func(c model.RunningContainer) bool {
		return c.Spec.Name == container || c.ID == container
	})
}

func (f *fakeDocker) list(w http.ResponseWriter, r *http.Request) {
	var spec map[string][]string
	if err := json.Unmarshal([]byte(r.URL.Query().Get("filters")), &spec); err != nil || len(spec["label"]) != 1 {
		refuse(w, http.StatusBadRequest, fmt.Sprintf("invalid filters: %v", err))
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != "" {
		refuse(w, http.StatusInternalServerError, f.failWith)
		return
	}
	listed := []map[string]any{}
	for _, container := range f.running {
		if _, ok := container.Spec.Labels[spec["label"][0]]; ok {
			listed = append(listed, map[string]any{"Id": container.ID, "Names": []string{"/" + container.Spec.Name}})
		}
	}
	answer(w, http.StatusOK, listed)
}

func (f *fakeDocker) inspect(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != "" {
		refuse(w, http.StatusInternalServerError, f.failWith)
		return
	}
	index := f.find(r.PathValue("id"))
	if index < 0 {
		refuse(w, http.StatusNotFound, "No such container: "+r.PathValue("id"))
		return
	}
	container := f.running[index]
	if health, ok := f.healthByName[container.Spec.Name]; ok {
		container.Health = health
	}
	if running, ok := f.runningByName[container.Spec.Name]; ok {
		container.Running = running
	}
	answer(w, http.StatusOK, inspectOf(container, f.extras[container.ID], requestVersion(r)))
}

// requestVersion is the Engine API version a request was made in.
func requestVersion(r *http.Request) string {
	version, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v"), "/")
	return version
}

// inspectOf is a container as Docker's inspect shows it in version. The
// config is what a create of it would say (Docker shows back what it was
// asked for), and the rest is the runtime state around it.
//
// What Docker adds of its own is there too, for the overrides to deal with:
// the short id as the hostname, its default masked and read-only paths, each
// network's operational data, and, before 1.45, the short id among a
// network's aliases. Anything else the container was created with - extras,
// by its id - is shown back as it was asked for, so what a recreate passes
// through comes back on the next inspect.
func inspectOf(container model.RunningContainer, extra inspectExtra, version string) map[string]any {
	created := typedPayload(container.Spec)
	config := map[string]any{}
	for key, value := range created {
		if key != "HostConfig" && key != "NetworkingConfig" {
			config[key] = value
		}
	}
	host := created["HostConfig"].(map[string]any)
	if len(container.ID) >= 12 {
		config["Hostname"] = container.ID[:12]
	}
	if !container.Spec.Privileged {
		host["MaskedPaths"] = []string{"/proc/kcore"}
		host["ReadonlyPaths"] = []string{"/proc/sys"}
	}
	overlay(config, extra.Config)
	overlay(host, extra.HostConfig)
	state := map[string]any{"Pid": container.PID, "Running": container.Running, "Restarting": false}
	if container.Health != model.NoHealth && container.Health != "" {
		state["Health"] = map[string]any{"Status": container.Health}
	}
	mounts := []map[string]any{}
	for _, mount := range container.Spec.Mounts {
		entry := map[string]any{"Type": mount.Type, "Source": mount.Source, "Destination": mount.Target, "RW": !mount.ReadOnly}
		if mount.Type == model.Volume {
			entry["Name"] = mount.Source
			entry["Source"] = "/var/lib/docker/volumes/" + mount.Source + "/_data"
		}
		mounts = append(mounts, entry)
	}
	networks := map[string]any{}
	for i, network := range container.Spec.Networks {
		endpoint := typedEndpoint(network)
		endpoint["IPAddress"] = network.IPAddress
		endpoint["NetworkID"] = "net-" + network.Name
		endpoint["EndpointID"] = fmt.Sprintf("ep-%s-%d", container.ID, i)
		endpoint["DNSNames"] = []string{container.Spec.Name}
		if len(container.ID) >= 12 && !atLeast(version, apiAliasesWithoutID) && network.Name != "bridge" {
			endpoint["Aliases"] = append(slices.Clone(network.Aliases), container.ID[:12])
		}
		overlay(endpoint, extra.Endpoints[network.Name])
		networks[network.Name] = endpoint
	}
	return map[string]any{
		"Id": container.ID, "Name": "/" + container.Spec.Name, "Image": container.ImageID,
		"State": state, "Config": config, "HostConfig": host, "Mounts": mounts,
		"NetworkSettings": map[string]any{"Networks": networks},
	}
}

// inspectExtra is what a container shows on inspect beyond its typed spec:
// what its create asked for, or a test gave it.
type inspectExtra struct {
	Config, HostConfig map[string]any
	Endpoints          map[string]map[string]any
}

// overlay adds what extra has that into does not.
func overlay(into, extra map[string]any) {
	for key, value := range extra {
		if _, set := into[key]; !set {
			into[key] = value
		}
	}
}

// extraOf is a create body as the extras it shows on inspect.
func extraOf(body []byte) inspectExtra {
	var asked struct {
		HostConfig       map[string]any
		NetworkingConfig struct {
			EndpointsConfig map[string]map[string]any
		}
	}
	var config map[string]any
	json.Unmarshal(body, &config)
	json.Unmarshal(body, &asked)
	delete(config, "HostConfig")
	delete(config, "NetworkingConfig")
	return inspectExtra{Config: config, HostConfig: asked.HostConfig, Endpoints: asked.NetworkingConfig.EndpointsConfig}
}

// specOf is the spec a create body asks for, read as Docker would show it
// back - through the client's own inspect mapping, so a field the create
// drops or garbles comes back wrong. Typed only: what the body passes
// through beyond that is the fake's extras.
func specOf(name string, body []byte) (model.ContainerSpec, error) {
	var asked struct {
		HostConfig struct {
			Binds  []string
			Mounts []struct {
				Type, Source, Target string
				ReadOnly             bool
			}
		}
		NetworkingConfig struct {
			EndpointsConfig map[string]json.RawMessage
		}
	}
	var whole map[string]json.RawMessage
	for _, into := range []any{&asked, &whole} {
		if err := json.Unmarshal(body, into); err != nil {
			return model.ContainerSpec{}, err
		}
	}
	mounts := []map[string]any{}
	for _, bind := range asked.HostConfig.Binds {
		parts := strings.Split(bind, ":")
		mounts = append(mounts, map[string]any{"Type": "bind", "Source": parts[0], "Destination": parts[1], "RW": len(parts) < 3 || !slices.Contains(strings.Split(parts[2], ","), "ro")})
	}
	for _, mount := range asked.HostConfig.Mounts {
		mounts = append(mounts, map[string]any{"Type": mount.Type, "Name": mount.Source, "Source": mount.Source, "Destination": mount.Target, "RW": !mount.ReadOnly})
	}
	hostConfig := whole["HostConfig"]
	delete(whole, "HostConfig")
	delete(whole, "NetworkingConfig")
	shown, _ := json.Marshal(map[string]any{
		"Name": "/" + name, "Config": whole, "HostConfig": hostConfig, "Mounts": mounts,
		"NetworkSettings": map[string]any{"Networks": asked.NetworkingConfig.EndpointsConfig},
	})
	var raw Inspected
	if err := json.Unmarshal(shown, &raw); err != nil {
		return model.ContainerSpec{}, err
	}
	spec := raw.Spec()
	spec.Inspected = nil
	return spec, nil
}

// misplacedMAC is a MAC address where version does not take one, or "":
// the fake refuses it, so a test proves none is sent there.
func misplacedMAC(body []byte, version string) string {
	var asked struct {
		MacAddress       string
		NetworkingConfig struct {
			EndpointsConfig map[string]struct{ MacAddress string }
		}
		EndpointConfig struct{ MacAddress string }
	}
	json.Unmarshal(body, &asked)
	switch {
	case asked.MacAddress != "" && atLeast(version, apiInspectEndpointMAC):
		return "Config.MacAddress"
	case asked.EndpointConfig.MacAddress != "" && !atLeast(version, apiEndpointMAC):
		return "EndpointSettings.MacAddress"
	}
	for _, endpoint := range asked.NetworkingConfig.EndpointsConfig {
		if endpoint.MacAddress != "" && !atLeast(version, apiEndpointMAC) {
			return "EndpointSettings.MacAddress"
		}
	}
	return ""
}

// typedPayload is a create body for a spec built in Go: how the fake
// renders its containers, and how a real-Docker test creates one to start
// from. Tidewaiter itself never builds one - it recreates from inspect.
func typedPayload(spec model.ContainerSpec) map[string]any {
	payload := map[string]any{
		"Image":  spec.Image,
		"Env":    orEmpty(spec.Env),
		"Labels": orEmptyMap(spec.Labels),
	}
	if spec.Cmd != nil {
		payload["Cmd"] = spec.Cmd
	}
	if spec.Entrypoint != nil {
		payload["Entrypoint"] = spec.Entrypoint
	}
	if len(spec.Exposed) > 0 {
		payload["ExposedPorts"] = set(spec.Exposed)
	}
	if len(spec.Volumes) > 0 {
		payload["Volumes"] = set(spec.Volumes)
	}
	setIf(payload, "User", spec.User)
	setIf(payload, "WorkingDir", spec.WorkingDir)
	setIf(payload, "StopSignal", spec.StopSignal)
	if spec.StopTimeout != nil {
		payload["StopTimeout"] = *spec.StopTimeout
	}
	if check := spec.Healthcheck; check != nil {
		healthcheck := map[string]any{}
		if len(check.Test) > 0 {
			healthcheck["Test"] = check.Test
		}
		for key, value := range map[string]int64{
			"Interval": int64(check.Interval), "Timeout": int64(check.Timeout), "Retries": int64(check.Retries),
			"StartPeriod": int64(check.StartPeriod), "StartInterval": int64(check.StartInterval),
		} {
			if value != 0 {
				healthcheck[key] = value
			}
		}
		payload["Healthcheck"] = healthcheck
	}
	binds, mounts := []string{}, []map[string]any{}
	for _, mount := range spec.Mounts {
		if mount.Type == model.Bind {
			bind := mount.Source + ":" + mount.Target
			if mount.ReadOnly {
				bind += ":ro"
			}
			binds = append(binds, bind)
			continue
		}
		long := map[string]any{"Type": mount.Type, "Target": mount.Target, "ReadOnly": mount.ReadOnly}
		if mount.Type == model.Volume {
			long["Source"] = mount.Source
		}
		mounts = append(mounts, long)
	}
	bindings := map[string][]map[string]string{}
	for _, port := range spec.Bindings {
		key := strconv.Itoa(port.ContainerPort) + "/" + string(port.Protocol)
		binding := map[string]string{"HostIp": port.HostIP, "HostPort": ""}
		switch {
		case port.HostRange != "":
			binding["HostPort"] = port.HostRange
		case port.HostPort != 0:
			binding["HostPort"] = strconv.Itoa(port.HostPort)
		}
		if !slices.ContainsFunc(bindings[key], func(other map[string]string) bool { return maps.Equal(other, binding) }) {
			bindings[key] = append(bindings[key], binding)
		}
	}
	host := map[string]any{"Binds": binds, "Mounts": mounts, "PortBindings": bindings, "Privileged": spec.Privileged}
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
		payload["NetworkingConfig"] = map[string]any{"EndpointsConfig": map[string]any{primary.Name: typedEndpoint(primary)}}
	}
	return payload
}

func setIf(into map[string]any, key, value string) {
	if value != "" {
		into[key] = value
	}
}

// typedEndpoint is the endpoint settings of a network attachment built in
// Go.
func typedEndpoint(network model.NetworkAttachment) map[string]any {
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

// inspected is spec with the inspect Docker would show of it, as a
// recreate reads it.
func inspected(t *testing.T, spec model.ContainerSpec, version string) model.ContainerSpec {
	t.Helper()
	var raw Inspected
	if err := json.Unmarshal([]byte(jsonOf(inspectOf(model.RunningContainer{ID: "0123456789abcdef", Spec: spec}, inspectExtra{}, version))), &raw); err != nil {
		t.Fatal(err)
	}
	raw.APIVersion = version
	spec.Inspected = raw.Spec().Inspected
	return spec
}

func (f *fakeDocker) image(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failImage != "" {
		refuse(w, http.StatusInternalServerError, f.failImage)
		return
	}
	name := r.PathValue("name")
	digest, ok := f.images[name]
	if !ok {
		refuse(w, http.StatusNotFound, "No such image: "+name)
		return
	}
	answer(w, http.StatusOK, map[string]any{"Id": "sha256:config", "RepoDigests": []string{"app@" + string(digest)}, "Config": imageConfigOf(f.configs[digest])})
}

// imageConfigOf is an image's Config as /images/{name}/json shows it: the
// same shape as a container's, so written the way a create of it would be.
func imageConfigOf(image ImageDefaults) map[string]any {
	config := typedPayload(model.ContainerSpec{
		Env: image.Env, Cmd: image.Cmd, Entrypoint: image.Entrypoint, Labels: image.Labels, Exposed: image.Exposed,
		User: image.User, WorkingDir: image.WorkingDir, StopSignal: image.StopSignal, Healthcheck: image.Healthcheck,
		Volumes: image.Volumes,
	})
	delete(config, "HostConfig")
	delete(config, "Image")
	return config
}

// merged is spec as Docker stores a container created from it on an image
// with these defaults: moby's merge of the two configs at create, field for
// field, so what a recreate leaves out comes back from the image it lands on.
func merged(spec model.ContainerSpec, image ImageDefaults) model.ContainerSpec {
	spec.Env = slices.Clone(spec.Env)
	if len(spec.Env) == 0 {
		spec.Env = slices.Clone(image.Env)
	} else {
		for _, entry := range image.Env {
			key, _, _ := strings.Cut(entry, "=")
			if !slices.ContainsFunc(spec.Env, func(own string) bool { ownKey, _, _ := strings.Cut(own, "="); return ownKey == key }) {
				spec.Env = append(spec.Env, entry)
			}
		}
	}
	spec.Env = orEmpty(spec.Env)
	spec.Labels = orEmptyMap(spec.Labels)
	for key, value := range image.Labels {
		if _, set := spec.Labels[key]; !set {
			spec.Labels[key] = value
		}
	}
	exposed := slices.Clone(spec.Exposed)
	for _, port := range image.Exposed {
		if !slices.Contains(exposed, port) {
			exposed = append(exposed, port)
		}
	}
	slices.Sort(exposed)
	spec.Exposed = orEmpty(exposed)
	if len(spec.Entrypoint) == 0 {
		if len(spec.Cmd) == 0 {
			spec.Cmd = image.Cmd
		}
		if spec.Entrypoint == nil {
			spec.Entrypoint = image.Entrypoint
		}
	}
	if check := image.Healthcheck; check != nil {
		if spec.Healthcheck == nil {
			spec.Healthcheck = check
		} else {
			own := *spec.Healthcheck
			if len(own.Test) == 0 {
				own.Test = check.Test
			}
			own.Interval = cmp.Or(own.Interval, check.Interval)
			own.Timeout = cmp.Or(own.Timeout, check.Timeout)
			own.Retries = cmp.Or(own.Retries, check.Retries)
			own.StartPeriod = cmp.Or(own.StartPeriod, check.StartPeriod)
			own.StartInterval = cmp.Or(own.StartInterval, check.StartInterval)
			spec.Healthcheck = &own
		}
	}
	volumes := slices.Clone(spec.Volumes)
	for _, volume := range image.Volumes {
		if !slices.Contains(volumes, volume) {
			volumes = append(volumes, volume)
		}
	}
	slices.Sort(volumes)
	spec.Volumes = orEmpty(volumes)
	spec.User = cmp.Or(spec.User, image.User)
	spec.WorkingDir = cmp.Or(spec.WorkingDir, image.WorkingDir)
	spec.StopSignal = cmp.Or(spec.StopSignal, image.StopSignal)
	return spec
}

func (f *fakeDocker) network(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	driver, ok := f.drivers[r.PathValue("name")]
	if !ok {
		driver = "bridge"
	}
	if driver == "" {
		refuse(w, http.StatusNotFound, "network "+r.PathValue("name")+" not found")
		return
	}
	answer(w, http.StatusOK, map[string]any{"Name": r.PathValue("name"), "Driver": driver})
}

func (f *fakeDocker) stats(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("stream") != "false" {
		refuse(w, http.StatusBadRequest, "a stats feed never ends; one snapshot was wanted")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.netBytesFail != "" {
		refuse(w, http.StatusConflict, f.netBytesFail)
		return
	}
	bytes := f.netBytes[r.PathValue("id")]
	answer(w, http.StatusOK, map[string]any{"networks": map[string]any{"eth0": map[string]uint64{"rx_bytes": bytes, "tx_bytes": 0}}})
}

// imageIDFor is the stable local image id a tag resolves to now; the caller
// holds mu.
func (f *fakeDocker) imageIDFor(ref string) string {
	if digest, ok := f.images[ref]; ok {
		return "config-" + string(digest)
	}
	return "config-" + ref
}

func (f *fakeDocker) pull(w http.ResponseWriter, r *http.Request) {
	ref := r.URL.Query().Get("fromImage")
	f.mu.Lock()
	f.pulls = append(f.pulls, pull{ref: ref, auth: r.Header.Get("X-Registry-Auth")})
	failure := f.failPull
	if landed, ok := f.pullLands[ref]; ok && failure == "" {
		f.images[ref] = landed
		f.images[f.imageIDFor(ref)] = landed
	}
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, "{\"status\":\"Pulling from %s\"}\n", ref)
	if failure != "" {
		json.NewEncoder(w).Encode(map[string]any{"errorDetail": map[string]string{"message": failure}, "error": failure})
		return
	}
	fmt.Fprintln(w, `{"status":"Download complete"}`)
}

func (f *fakeDocker) failing(w http.ResponseWriter, op Op) bool {
	if message := f.failOp[op]; message != "" {
		refuse(w, http.StatusInternalServerError, message)
		return true
	}
	return false
}

func (f *fakeDocker) create(w http.ResponseWriter, r *http.Request) {
	var body json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || r.Header.Get("Content-Type") != "application/json" {
		refuse(w, http.StatusBadRequest, fmt.Sprintf("invalid create request: %v", err))
		return
	}
	if field := misplacedMAC(body, requestVersion(r)); field != "" {
		refuse(w, http.StatusBadRequest, fmt.Sprintf("%s is not taken in API v%s", field, requestVersion(r)))
		return
	}
	name := r.URL.Query().Get("name")
	spec, err := specOf(name, body)
	if err != nil {
		refuse(w, http.StatusBadRequest, err.Error())
		return
	}
	var payload map[string]any
	json.Unmarshal(body, &payload)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failing(w, Create) {
		return
	}
	if f.find(name) >= 0 {
		refuse(w, http.StatusConflict, fmt.Sprintf("Conflict. The container name \"/%s\" is already in use", name))
		return
	}
	// Created from whatever image the tag points at here now - by its id, so
	// a later lookup of what it runs is stable even if the tag moves again -
	// and with that image's defaults filled in, as Docker shows it after.
	spec = merged(spec, f.configs[f.images[spec.Image]])
	f.creates = append(f.creates, spec)
	f.payloads = append(f.payloads, payload)
	id := fmt.Sprintf("id-%s-%d", name, len(f.creates))
	f.running = append(f.running, model.RunningContainer{
		ID: id, PID: 1000 + len(f.creates), Spec: spec, ImageID: f.imageIDFor(spec.Image), Health: model.NoHealth, Running: true,
	})
	f.extras[id] = extraOf(body)
	answer(w, http.StatusCreated, map[string]any{"Id": id, "Warnings": []string{}})
}

// lifecycle is a call on one existing container.
func (f *fakeDocker) lifecycle(w http.ResponseWriter, r *http.Request, op Op, act func(index int)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failing(w, op) {
		return
	}
	index := f.find(r.PathValue("id"))
	if index < 0 {
		refuse(w, http.StatusNotFound, "No such container: "+r.PathValue("id"))
		return
	}
	act(index)
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeDocker) start(w http.ResponseWriter, r *http.Request) {
	f.lifecycle(w, r, Start, func(int) { f.starts = append(f.starts, r.PathValue("id")) })
}

func (f *fakeDocker) stop(w http.ResponseWriter, r *http.Request) {
	grace, err := strconv.Atoi(r.URL.Query().Get("t"))
	if err != nil {
		refuse(w, http.StatusBadRequest, "invalid t: "+r.URL.Query().Get("t"))
		return
	}
	f.lifecycle(w, r, Stop, func(int) { f.stops = append(f.stops, stopped{r.PathValue("id"), grace}) })
}

func (f *fakeDocker) rename(w http.ResponseWriter, r *http.Request) {
	to := r.URL.Query().Get("name")
	f.lifecycle(w, r, Rename, func(index int) {
		f.renames = append(f.renames, renamed{r.PathValue("id"), to})
		f.running[index].Spec.Name = to
	})
}

func (f *fakeDocker) remove(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("v") != "1" {
		refuse(w, http.StatusBadRequest, "anonymous volumes would be left behind")
		return
	}
	force := r.URL.Query().Get("force") == "1"
	f.lifecycle(w, r, Remove, func(index int) {
		f.removes = append(f.removes, removed{r.PathValue("id"), force})
		f.running = slices.Delete(f.running, index, index+1)
	})
}

func (f *fakeDocker) removeImage(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removedImages = append(f.removedImages, r.PathValue("name"))
	answer(w, http.StatusOK, []map[string]string{{"Untagged": r.PathValue("name")}})
}

func (f *fakeDocker) connect(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Container      string
		EndpointConfig map[string]any
	}
	data, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(data, &body); err != nil {
		refuse(w, http.StatusBadRequest, err.Error())
		return
	}
	if field := misplacedMAC(data, requestVersion(r)); field != "" {
		refuse(w, http.StatusBadRequest, fmt.Sprintf("%s is not taken in API v%s", field, requestVersion(r)))
		return
	}
	var endpoint InspectedNetwork
	json.Unmarshal([]byte(jsonOf(body.EndpointConfig)), &endpoint)
	f.mu.Lock()
	defer f.mu.Unlock()
	index := f.find(body.Container)
	if index < 0 {
		refuse(w, http.StatusNotFound, "No such container: "+body.Container)
		return
	}
	network := networksOf(map[string]InspectedNetwork{r.PathValue("name"): endpoint}, "")[0]
	f.connects = append(f.connects, connected{body.Container, network, body.EndpointConfig})
	f.running[index].Spec.Networks = append(slices.Clone(f.running[index].Spec.Networks), network)
	w.WriteHeader(http.StatusOK)
}

func (f *fakeDocker) events(w http.ResponseWriter, r *http.Request) {
	var spec map[string][]string
	if err := json.Unmarshal([]byte(r.URL.Query().Get("filters")), &spec); err != nil {
		refuse(w, http.StatusBadRequest, fmt.Sprintf("invalid filters: %v", err))
		return
	}
	f.mu.Lock()
	id := f.opened
	f.opened++
	drop := make(chan struct{})
	f.streams[id] = drop
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.streams, id)
	}()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.(http.Flusher).Flush()
	for {
		f.mu.Lock()
		var next Event
		queued := len(f.queue) > 0
		if queued {
			next, f.queue = f.queue[0], f.queue[1:]
		}
		f.mu.Unlock()
		if !queued {
			select {
			case <-f.wake:
			case <-drop:
				return
			case <-r.Context().Done():
				return
			}
			continue
		}
		kind, _, _ := strings.Cut(next.Action, ":")
		if !slices.Contains(spec["event"], kind) {
			continue
		}
		line, _ := json.Marshal(map[string]any{
			"status": next.Action, "id": "id-" + next.Container, "Type": "container", "Action": next.Action,
			"Actor": map[string]any{"ID": "id-" + next.Container, "Attributes": map[string]string{"name": next.Container, "image": "app:latest"}},
		})
		if _, err := w.Write(append(line, '\n')); err != nil {
			return
		}
		w.(http.Flusher).Flush()
	}
}

// fakeRegistry is a registry as an http.Handler: what each ref's tag
// resolves to, a 404 for one it does not know.
type fakeRegistry struct {
	mu       sync.Mutex
	digests  map[string]model.Digest
	failWith string
	calls    []string
}

func newFakeRegistry() *fakeRegistry { return &fakeRegistry{digests: map[string]model.Digest{}} }

func (f *fakeRegistry) set(ref string, digest model.Digest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.digests[ref] = digest
}

func (f *fakeRegistry) fail(message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failWith = message
}

func (f *fakeRegistry) asked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Host+r.URL.Path)
	if f.failWith != "" {
		http.Error(w, f.failWith, http.StatusServiceUnavailable)
		return
	}
	for _, ref := range slices.Sorted(maps.Keys(f.digests)) {
		image := registry.ParseRef(ref)
		if r.Host == image.Registry && r.URL.Path == "/v2/"+image.Repository+"/manifests/"+image.Reference {
			w.Header().Set("Docker-Content-Digest", string(f.digests[ref]))
			return
		}
	}
	http.Error(w, `{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`, http.StatusNotFound)
}

// spec is a ContainerSpec with defaults, opted in to the registry policy.
func spec(name string) model.ContainerSpec {
	return model.ContainerSpec{
		Name: name, Image: "app:latest", Env: []string{},
		Labels: map[string]string{AutoupdateLabel: "registry"}, Mounts: []model.Mount{}, Bindings: []model.PortBinding{},
		Exposed: []string{}, Volumes: []string{}, Networks: []model.NetworkAttachment{}, CapAdd: []string{}, CapDrop: []string{},
		Devices: []model.Device{}, ExtraHosts: []string{}, Sysctls: map[string]string{},
	}
}

// container is a running container with defaults.
func container(name string, change ...func(*model.RunningContainer)) model.RunningContainer {
	running := model.RunningContainer{ID: "id-" + name, PID: 4242, ImageID: "config-current", Health: model.NoHealth, Running: true, Spec: spec(name)}
	for _, apply := range change {
		apply(&running)
	}
	return running
}

func port(host, container int, protocol model.Protocol) model.PublishedPort {
	return model.PublishedPort{HostPort: host, ContainerPort: container, Protocol: protocol}
}

// bound is ports as bindings on every interface, as a plain `-p` makes them.
func bound(ports ...model.PublishedPort) []model.PortBinding {
	bindings := []model.PortBinding{}
	for _, port := range ports {
		bindings = append(bindings, model.PortBinding{PublishedPort: port})
	}
	return bindings
}
