package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/danielbodart/tidewaiter/internal/systemd"
)

// output is a stdout or stderr that a test reads while Main is still writing.
type output struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.Write(p)
}

func (o *output) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

func (o *output) Lines() []string {
	return strings.Split(strings.TrimSuffix(o.String(), "\n"), "\n")
}

// fakeDocker is as much of the Engine API as the command line reaches on its
// own: listing, inspecting and an image's digest. What a swap does to
// Docker is the docker package's to prove, against its own fake.
type fakeDocker struct {
	mu sync.Mutex
	// By id, each an inspect document.
	containers map[string]map[string]any
	// Manifest digests of the local images, by ref.
	images map[string]string
	// The label filter of every listing.
	listed []string
	// When set, a listing waits on it - a pass caught in flight.
	hold chan struct{}
	// When set, a listing fails with it.
	failure string
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{containers: map[string]map[string]any{}, images: map[string]string{}}
}

// run is an opted-in container, running image.
func (d *fakeDocker) run(name, image string, labels map[string]string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	id := strings.Repeat(string(rune('a'+len(d.containers))), 64)
	d.containers[id] = map[string]any{
		"Id":    id,
		"Name":  "/" + name,
		"Image": "sha256:" + strings.Repeat("c", 64),
		"State": map[string]any{"Pid": 4242, "Running": true},
		"Config": map[string]any{
			"Image":  image,
			"Labels": labels,
		},
		"HostConfig":      map[string]any{"NetworkMode": "bridge"},
		"NetworkSettings": map[string]any{"Networks": map[string]any{"bridge": map[string]any{}}},
	}
}

func (d *fakeDocker) listings() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.listed)
}

var (
	listing = regexp.MustCompile(`^(?:/v[0-9.]+)?/containers/json$`)
	inspect = regexp.MustCompile(`^(?:/v[0-9.]+)?/containers/([^/]+)/json$`)
	image   = regexp.MustCompile(`^(?:/v[0-9.]+)?/images/(.+)/json$`)
)

func (d *fakeDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case r.Method == http.MethodGet && listing.MatchString(path):
		var filters map[string][]string
		_ = json.Unmarshal([]byte(r.URL.Query().Get("filters")), &filters)
		d.mu.Lock()
		d.listed = append(d.listed, strings.Join(filters["label"], ","))
		hold, failure := d.hold, d.failure
		d.mu.Unlock()
		if hold != nil {
			<-hold
		}
		if failure != "" {
			reply(w, http.StatusInternalServerError, map[string]string{"message": failure})
			return
		}
		d.mu.Lock()
		var ids []map[string]string
		for id := range d.containers {
			ids = append(ids, map[string]string{"Id": id})
		}
		d.mu.Unlock()
		reply(w, http.StatusOK, ids)
	case r.Method == http.MethodGet && inspect.MatchString(path):
		d.mu.Lock()
		found, ok := d.containers[inspect.FindStringSubmatch(path)[1]]
		d.mu.Unlock()
		if !ok {
			reply(w, http.StatusNotFound, map[string]string{"message": "No such container"})
			return
		}
		reply(w, http.StatusOK, found)
	case r.Method == http.MethodGet && image.MatchString(path):
		ref := image.FindStringSubmatch(path)[1]
		d.mu.Lock()
		digest, ok := d.images[ref]
		d.mu.Unlock()
		if !ok {
			reply(w, http.StatusNotFound, map[string]string{"message": "No such image: " + ref})
			return
		}
		repository, _, _ := strings.Cut(ref, ":")
		reply(w, http.StatusOK, map[string]any{"RepoDigests": []string{repository + "@" + digest}})
	default:
		reply(w, http.StatusNotFound, map[string]string{"message": "the fake has no " + r.Method + " " + path})
	}
}

func reply(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// fakeBus is systemd, with units in memory.
type fakeBus struct {
	mu      sync.Mutex
	units   map[string]systemd.UnitState
	reloads int
	closed  bool
}

func (b *fakeBus) Unit(_ context.Context, name string) (systemd.UnitState, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if state, ok := b.units[name]; ok {
		return state, nil
	}
	return systemd.UnitState{Name: name, LoadState: "not-found", ActiveState: "inactive", SubState: "dead"}, nil
}

func (b *fakeBus) Restart(context.Context, string) (systemd.JobResult, error) {
	return "", errors.New("the CLI's fake bus restarts nothing")
}

func (b *fakeBus) DaemonReload(context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reloads++
	return nil
}

func (b *fakeBus) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	return nil
}

// fakeNix answers for nix-env and nix-store, and remembers what was run.
type fakeNix struct {
	mu   sync.Mutex
	runs []string
}

func (n *fakeNix) Run(_ context.Context, program string, args ...string) ([]byte, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.runs = append(n.runs, program+" "+strings.Join(args, " "))
	if program == "nix-store" {
		return []byte(args[len(args)-1] + "\n/nix/store/aaa-glibc\n"), nil
	}
	return nil, nil
}

func (n *fakeNix) ran() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.runs)
}
