package systemd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/danielbodart/gokit/sockdiag"
	"github.com/danielbodart/tidewaiter/internal/statestore"
)

var ctx = context.Background()

func assertEqual[T any](t *testing.T, got, want T) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %#v\nwant %#v", got, want)
	}
}

func ref(n int) *int { return &n }

// journal is what every fake below was asked to do, in one list, so a test
// can assert the ORDER across them - closure bound before the reload, the
// reload before the profile moves, the profile before the restart.
type journal struct {
	lines []string
}

func (j *journal) add(format string, args ...any) {
	j.lines = append(j.lines, fmt.Sprintf(format, args...))
}

// starting is the lines that start with prefix.
func (j *journal) starting(prefix string) []string {
	var found []string
	for _, line := range j.lines {
		if len(line) >= len(prefix) && line[:len(prefix)] == prefix {
			found = append(found, line)
		}
	}
	return found
}

// unitState is a running unit, as systemd would describe it.
func unitState(name string) UnitState {
	return UnitState{
		Name:         name,
		LoadState:    "loaded",
		ActiveState:  "active",
		SubState:     "running",
		Result:       "success",
		MainPID:      4242,
		ControlGroup: "/system.slice/" + name,
		InvocationID: "0001",
	}
}

// fakeSystemd is systemd in memory: units by name, and a restart that, by
// default, brings the unit back up healthy on a new invocation. onRestart
// replaces that for a test that wants a failed job, a crash, or a unit that
// never binds; it may change the unit's state and returns the job's result.
type fakeSystemd struct {
	journal *journal
	units   map[string]UnitState
	// Replaces what a restart does.
	onRestart func(name string, fake *fakeSystemd) JobResult
	// Makes the next restart fail, as a job that could not be queued does.
	restartFails error
	// Makes every read fail.
	fails       error
	reloadFails error
	// Called before each read, for a test that looks at the world mid-pass.
	beforeUnit  func(name string)
	invocations int
}

var _ Bus = (*fakeSystemd)(nil)

func newFakeSystemd(journal *journal) *fakeSystemd {
	return &fakeSystemd{journal: journal, units: map[string]UnitState{}, invocations: 1}
}

func (f *fakeSystemd) add(state UnitState) *fakeSystemd {
	f.units[state.Name] = state
	return f
}

// patch changes a unit's state in place.
func (f *fakeSystemd) patch(name string, change func(*UnitState)) {
	state := f.units[name]
	change(&state)
	f.units[name] = state
}

// nextInvocation is a fresh invocation id, as each start gets.
func (f *fakeSystemd) nextInvocation() string {
	f.invocations++
	return fmt.Sprintf("%04d", f.invocations)
}

func (f *fakeSystemd) Unit(_ context.Context, name string) (UnitState, error) {
	if f.beforeUnit != nil {
		f.beforeUnit(name)
	}
	if f.fails != nil {
		return UnitState{}, f.fails
	}
	if state, ok := f.units[name]; ok {
		return state, nil
	}
	return UnitState{Name: name, LoadState: "not-found", ActiveState: "inactive", SubState: "dead", Result: "success"}, nil
}

func (f *fakeSystemd) Restart(_ context.Context, name string) (JobResult, error) {
	f.journal.add("restart %s", name)
	if err := f.restartFails; err != nil {
		f.restartFails = nil
		return "", err
	}
	if f.onRestart != nil {
		return f.onRestart(name, f), nil
	}
	f.patch(name, func(state *UnitState) {
		state.ActiveState, state.SubState, state.Result, state.InvocationID = "active", "running", "success", f.nextInvocation()
	})
	return JobDone, nil
}

func (f *fakeSystemd) DaemonReload(context.Context) error {
	f.journal.add("daemon-reload")
	return f.reloadFails
}

type nixProfile struct {
	generations map[int]string
	current     int
}

// fakeProfiles is Nix profiles in memory: numbered generations per profile,
// the current one, and nix-env's numbering (a new generation is one past the
// highest).
type fakeProfiles struct {
	journal  *journal
	profiles map[string]*nixProfile
	// Makes the next Set fail, as nix-env does when the Nix daemon is
	// unreachable.
	setFails error
	// Called before each Set.
	beforeSet func()
}

func newFakeProfiles(journal *journal) *fakeProfiles {
	return &fakeProfiles{journal: journal, profiles: map[string]*nixProfile{}}
}

// seed sets a profile's generations 1..n to these paths, the last current.
func (f *fakeProfiles) seed(name string, paths ...string) *fakeProfiles {
	generations := map[int]string{}
	for i, path := range paths {
		generations[i+1] = path
	}
	f.profiles[name] = &nixProfile{generations: generations, current: len(paths)}
	return f
}

func (f *fakeProfiles) generationsOf(name string) []int {
	if f.profiles[name] == nil {
		return nil
	}
	return slices.Sorted(maps.Keys(f.profiles[name].generations))
}

func (f *fakeProfiles) currentGeneration(name string) int {
	if f.profiles[name] == nil {
		return 0
	}
	return f.profiles[name].current
}

func (f *fakeProfiles) Current(_ context.Context, name string) (string, error) {
	if p := f.profiles[name]; p != nil {
		return p.generations[p.current], nil
	}
	return "", nil
}

func (f *fakeProfiles) Generation(_ context.Context, name string) (int, error) {
	return f.currentGeneration(name), nil
}

func (f *fakeProfiles) Target(_ context.Context, name string, generation int) (string, error) {
	if p := f.profiles[name]; p != nil {
		return p.generations[generation], nil
	}
	return "", nil
}

func (f *fakeProfiles) Set(_ context.Context, name, path string) error {
	if f.beforeSet != nil {
		f.beforeSet()
	}
	f.journal.add("set %s", path)
	if err := f.setFails; err != nil {
		f.setFails = nil
		return err
	}
	p := f.profiles[name]
	if p == nil {
		p = &nixProfile{generations: map[int]string{}}
		f.profiles[name] = p
	}
	next := 1
	for generation := range p.generations {
		next = max(next, generation+1)
	}
	p.generations[next] = path
	p.current = next
	return nil
}

func (f *fakeProfiles) SwitchGeneration(_ context.Context, name string, generation int) error {
	f.journal.add("switch %d", generation)
	p := f.profiles[name]
	if _, ok := p.generations[generation]; !ok {
		return fmt.Errorf("profile version %d does not exist", generation)
	}
	p.current = generation
	return nil
}

func (f *fakeProfiles) DeleteGeneration(_ context.Context, name string, generation int) error {
	f.journal.add("delete %d", generation)
	if p := f.profiles[name]; generation != p.current {
		delete(p.generations, generation)
	}
	return nil
}

func (f *fakeProfiles) Prune(_ context.Context, name string, keep int) error {
	f.journal.add("prune +%d", keep+1)
	p := f.profiles[name]
	newest := slices.Sorted(maps.Keys(p.generations))
	slices.Reverse(newest)
	newest = newest[:min(len(newest), keep+1)]
	for generation := range p.generations {
		if !slices.Contains(newest, generation) && generation != p.current {
			delete(p.generations, generation)
		}
	}
	return nil
}

type dropIn struct{ profile, target string }

// fakeClosure records each drop-in it would write; dropIns is what is on
// disk, per unit.
type fakeClosure struct {
	journal *journal
	dropIns map[string]dropIn
}

func newFakeClosure(journal *journal) *fakeClosure {
	return &fakeClosure{journal: journal, dropIns: map[string]dropIn{}}
}

func (f *fakeClosure) Bind(_ context.Context, unit, profile, target string) error {
	f.journal.add("bind %s", target)
	f.dropIns[unit] = dropIn{profile, target}
	return nil
}

func (f *fakeClosure) Ensure(ctx context.Context, unit, profile, target string) (bool, error) {
	if existing, ok := f.dropIns[unit]; ok && existing.profile == profile {
		return false, nil
	}
	return true, f.Bind(ctx, unit, profile, target)
}

// fakeSockets is the host's listening sockets, by the cgroup of the unit that
// made each.
type fakeSockets struct {
	listeners []sockdiag.Listener
}

// bind adds a UDP socket in a unit's cgroup.
func (f *fakeSockets) bind(unit string, port int) *fakeSockets {
	f.listeners = append(f.listeners, sockdiag.Listener{Protocol: sockdiag.UDP, Port: port, Cgroup: "/system.slice/" + unit})
	return f
}

func (f *fakeSockets) clear() { f.listeners = nil }

func (f *fakeSockets) Listening(context.Context) ([]sockdiag.Listener, error) {
	return slices.Clone(f.listeners), nil
}

// fakeUnits is the units directory, in memory: file name to contents.
type fakeUnits struct {
	files map[string]string
}

func newFakeUnits() *fakeUnits { return &fakeUnits{files: map[string]string{}} }

func (f *fakeUnits) declare(unit string, fields map[string]any) *fakeUnits {
	all := map[string]any{"unit": unit}
	maps.Copy(all, fields)
	text, err := json.Marshal(all)
	if err != nil {
		panic(err)
	}
	f.files[unit+".json"] = string(text)
	return f
}

func (f *fakeUnits) Read() ([]UnitFile, error) {
	var files []UnitFile
	for _, name := range slices.Sorted(maps.Keys(f.files)) {
		files = append(files, UnitFile{Name: name, Text: []byte(f.files[name])})
	}
	return files, nil
}

// memoryStore is the state file, in memory, kept as the JSON it would hold
// so nothing the source does later can change what was saved.
type memoryStore struct {
	mu    sync.Mutex
	text  []byte
	saves int
	fails error
}

var _ statestore.Store = (*memoryStore)(nil)

func storeOf(memories statestore.Memories) *memoryStore {
	store := &memoryStore{}
	store.text, _ = json.Marshal(memories)
	return store
}

func (m *memoryStore) Load() (statestore.Memories, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	memories := statestore.Memories{}
	if m.text != nil {
		if err := json.Unmarshal(m.text, &memories); err != nil {
			return nil, err
		}
	}
	return memories, nil
}

func (m *memoryStore) Save(memories statestore.Memories) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fails != nil {
		return m.fails
	}
	m.saves++
	m.text, _ = json.Marshal(memories)
	return nil
}

// memory is one unit's memory as last saved.
func (m *memoryStore) memory(unit string) statestore.UnitMemory {
	memories, _ := m.Load()
	return memories[unit]
}

// ticking is a clock every read of which moves it on a quarter second, and
// every wait on which is over at once, having moved it on by the wait - so a
// health gate's grace and deadline pass in a handful of polls, with nothing
// waiting on real time.
type ticking struct {
	mu  sync.Mutex
	now time.Time
}

func (c *ticking) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(250 * time.Millisecond)
	return c.now
}

func (c *ticking) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	fired := make(chan time.Time, 1)
	fired <- c.now
	return fired
}

var errBusGone = errors.New("bus gone")
