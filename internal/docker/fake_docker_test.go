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
}

func newFakeDocker() *fakeDocker {
	f := &fakeDocker{
		images: map[string]model.Digest{}, configs: map[model.Digest]ImageDefaults{}, pullLands: map[string]model.Digest{}, failOp: map[Op]string{},
		healthByName: map[string]model.HealthStatus{}, runningByName: map[string]bool{},
		drivers: map[string]string{}, netBytes: map[string]uint64{},
		wake: make(chan struct{}, 1), streams: map[int]chan struct{}{},
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
	answer(w, http.StatusOK, inspectOf(container, requestVersion(r)))
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
// A network's MacAddress is the one in use, and before 1.52 the one asked
// for on the container's own network is Config.MacAddress too, as moby
// shows them.
func inspectOf(container model.RunningContainer, version string) map[string]any {
	created := CreatePayload(container.Spec, NewestAPI)
	config := map[string]any{}
	for key, value := range created {
		if key != "HostConfig" && key != "NetworkingConfig" {
			config[key] = value
		}
	}
	if networks := container.Spec.Networks; len(networks) > 0 && networks[0].MacAddress != "" && !atLeast(version, apiInspectEndpointMAC) {
		config["MacAddress"] = networks[0].MacAddress
	}
	host := created["HostConfig"].(map[string]any)
	if !atLeast(version, apiUmask) {
		delete(host, "Umask")
	}
	state := map[string]any{"Pid": container.PID, "Running": container.Running, "Restarting": false}
	if container.Health != model.NoHealth && container.Health != "" {
		state["Health"] = map[string]any{"Status": container.Health}
	}
	mounts := []map[string]any{}
	for _, mount := range container.Spec.Mounts {
		entry := map[string]any{"Type": mount.Type, "Source": mount.Source, "Destination": mount.Target, "RW": !mount.ReadOnly}
		if mount.Type == model.Bind {
			entry["Propagation"] = cmp.Or(mount.Bind.Propagation, "rprivate")
		}
		if mount.Type == model.Image {
			// The mounted view names the image by id, not as it was asked.
			entry["Source"] = "/var/lib/docker/image-mounts/" + mount.Target
		}
		if mount.Type == model.Volume {
			entry["Name"] = mount.Source
			entry["Source"] = "/var/lib/docker/volumes/" + mount.Source + "/_data"
		}
		mounts = append(mounts, entry)
	}
	networks := map[string]any{}
	for _, network := range container.Spec.Networks {
		endpoint := endpointOf(network, NewestAPI, false)
		endpoint["IPAddress"] = network.IPAddress
		networks[network.Name] = endpoint
	}
	return map[string]any{
		"Id": container.ID, "Name": "/" + container.Spec.Name, "Image": container.ImageID,
		"State": state, "Config": config, "HostConfig": host, "Mounts": mounts,
		"NetworkSettings": map[string]any{"Networks": networks},
	}
}

// specOf is the spec a create payload asks for in version, read as Docker
// would show it back in that version - through the client's own inspect
// mapping, so a field the create drops or garbles comes back wrong.
func specOf(name string, body []byte, version string) (model.ContainerSpec, error) {
	raw := Inspected{APIVersion: version}
	var asked struct {
		HostConfig struct {
			Binds  []string
			Mounts []HostMount
		}
		NetworkingConfig struct {
			EndpointsConfig map[string]InspectedNetwork
		}
	}
	for _, into := range []any{&raw.Config, &asked} {
		if err := json.Unmarshal(body, into); err != nil {
			return model.ContainerSpec{}, err
		}
	}
	var hostConfig struct{ HostConfig json.RawMessage }
	json.Unmarshal(body, &hostConfig)
	if err := json.Unmarshal(hostConfig.HostConfig, &raw.HostConfig); err != nil {
		return model.ContainerSpec{}, err
	}
	raw.Name = "/" + name
	raw.Mounts = []InspectedMount{}
	for _, bind := range asked.HostConfig.Binds {
		parts := strings.Split(bind, ":")
		mount := InspectedMount{Type: "bind", Source: parts[0], Destination: parts[1], RW: true, Propagation: "rprivate"}
		if len(parts) > 2 {
			for _, option := range strings.Split(parts[2], ",") {
				switch option {
				case "ro":
					mount.RW = false
				case "shared", "rshared", "slave", "rslave", "private", "rprivate":
					mount.Propagation = option
				}
			}
		}
		raw.Mounts = append(raw.Mounts, mount)
	}
	for _, mount := range asked.HostConfig.Mounts {
		shown := InspectedMount{Type: mount.Type, Name: mount.Source, Source: mount.Source, Destination: mount.Target, RW: !mount.ReadOnly}
		if mount.Type == string(model.Bind) {
			shown.Propagation = "rprivate"
			if mount.BindOptions != nil && mount.BindOptions.Propagation != "" {
				shown.Propagation = mount.BindOptions.Propagation
			}
		}
		raw.Mounts = append(raw.Mounts, shown)
	}
	raw.NetworkSettings.Networks = asked.NetworkingConfig.EndpointsConfig
	// The MAC asked for, wherever this version takes it, shown where this
	// version shows it.
	for _, endpoint := range raw.NetworkSettings.Networks {
		raw.Config.MacAddress = cmp.Or(raw.Config.MacAddress, endpoint.MacAddress)
	}
	if atLeast(version, apiInspectEndpointMAC) {
		raw.Config.MacAddress = ""
	}
	return raw.Spec(), nil
}

// tooNew is the first field of a create or connect body that version does
// not know, or "": the fake refuses those, so a test proves none is sent.
func tooNew(body []byte, version string) string {
	var asked struct {
		MacAddress  string
		Healthcheck map[string]any
		HostConfig  struct {
			Annotations map[string]string
			Umask       *uint32
			Mounts      []HostMount
		}
		NetworkingConfig struct {
			EndpointsConfig map[string]InspectedNetwork
		}
		EndpointConfig *InspectedNetwork
	}
	json.Unmarshal(body, &asked)
	// A field set in body, known from since, and no longer from until.
	type known struct {
		set                bool
		name, since, until string
	}
	var fields []known
	field := func(set bool, name, since, until string) { fields = append(fields, known{set, name, since, until}) }
	field(asked.MacAddress != "", "Config.MacAddress", "", apiInspectEndpointMAC)
	_, startInterval := asked.Healthcheck["StartInterval"]
	field(startInterval, "Healthcheck.StartInterval", apiStartInterval, "")
	field(asked.HostConfig.Annotations != nil, "HostConfig.Annotations", apiAnnotations, "")
	field(asked.HostConfig.Umask != nil, "HostConfig.Umask", apiUmask, "")
	for _, mount := range asked.HostConfig.Mounts {
		field(mount.Type == string(model.Image), "Mount type image", apiImageMount, "")
		if options := mount.BindOptions; options != nil {
			field(options.NonRecursive, "BindOptions.NonRecursive", apiBindNonRecursive, "")
			field(options.CreateMountpoint, "BindOptions.CreateMountpoint", apiCreateMountpoint, "")
			field(options.ReadOnlyNonRecursive || options.ReadOnlyForceRecursive, "BindOptions.ReadOnly*Recursive", apiReadOnlyRecursive, "")
		}
		if options := mount.VolumeOptions; options != nil {
			field(options.Subpath != "", "VolumeOptions.Subpath", apiVolumeSubpath, "")
		}
		if options := mount.TmpfsOptions; options != nil {
			field(options.Options != nil, "TmpfsOptions.Options", apiTmpfsOptions, "")
		}
	}
	endpoints := slices.Collect(maps.Values(asked.NetworkingConfig.EndpointsConfig))
	if asked.EndpointConfig != nil {
		endpoints = append(endpoints, *asked.EndpointConfig)
	}
	for _, endpoint := range endpoints {
		field(endpoint.MacAddress != "", "EndpointSettings.MacAddress", apiEndpointMAC, "")
		field(endpoint.GwPriority != 0, "EndpointSettings.GwPriority", apiGwPriority, "")
	}
	for _, f := range fields {
		if f.set && (f.since != "" && !atLeast(version, f.since) || f.until != "" && atLeast(version, f.until)) {
			return f.name
		}
	}
	return ""
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
	config := CreatePayload(model.ContainerSpec{
		Env: image.Env, Cmd: image.Cmd, Entrypoint: image.Entrypoint, Labels: image.Labels, Exposed: image.Exposed,
		User: image.User, WorkingDir: image.WorkingDir, StopSignal: image.StopSignal, Healthcheck: image.Healthcheck,
	}, NewestAPI)
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
	if field := tooNew(body, requestVersion(r)); field != "" {
		refuse(w, http.StatusBadRequest, fmt.Sprintf("%s is not in API v%s", field, requestVersion(r)))
		return
	}
	name := r.URL.Query().Get("name")
	spec, err := specOf(name, body, requestVersion(r))
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
		EndpointConfig InspectedNetwork
	}
	data, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(data, &body); err != nil {
		refuse(w, http.StatusBadRequest, err.Error())
		return
	}
	if field := tooNew(data, requestVersion(r)); field != "" {
		refuse(w, http.StatusBadRequest, fmt.Sprintf("%s is not in API v%s", field, requestVersion(r)))
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	index := f.find(body.Container)
	if index < 0 {
		refuse(w, http.StatusNotFound, "No such container: "+body.Container)
		return
	}
	// As inspect shows it from 1.52, where a network's MacAddress is its own.
	network := networksOf(map[string]InspectedNetwork{r.PathValue("name"): body.EndpointConfig}, "", NewestAPI, "")[0]
	f.connects = append(f.connects, connected{body.Container, network})
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
		Exposed: []string{}, Networks: []model.NetworkAttachment{}, CapAdd: []string{}, CapDrop: []string{},
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
