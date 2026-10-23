package fakes

import (
	"context"
	"slices"
	"sync"

	"github.com/danielbodart/tidewaiter/internal/detect"
	"github.com/danielbodart/tidewaiter/internal/source"
	"github.com/danielbodart/tidewaiter/internal/status"
)

// Source reports whatever a test tells it to.
//
// It stands for whatever source a test is not about, so the daemon can be
// driven without Docker or systemd. Outcomes are what each pass returns,
// Activities what each tick does; Holds names workloads to hold mid-pass, and
// SeenSwapping records what the status said was swapping at that moment, so a
// test can prove it said so before the "swap".
type Source struct {
	mu          sync.Mutex
	name        source.Name
	status      *Status
	outcomes    []source.Outcome
	activities  []source.Activity
	holds       []string
	err         error
	activityErr error
	during      func(ctx context.Context)
	passes      []source.Pass
	ticks       [][]detect.ConntrackEntry
	swapping    [][]string
}

// NewSource is a source with no workloads. status, when not nil, is read at
// each hold for SeenSwapping.
func NewSource(name source.Name, status *Status) *Source {
	return &Source{name: name, status: status}
}

// Report replaces what every pass from now returns.
func (s *Source) Report(outcomes ...source.Outcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outcomes = outcomes
}

// Active replaces what every activity tick from now returns.
func (s *Source) Active(activities ...source.Activity) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activities = activities
}

// Hold makes every pass hold these workloads, as a swap would, before it
// returns.
func (s *Source) Hold(workloads ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.holds = workloads
}

// FailWith makes passes and ticks fail with err, until it is called with nil.
func (s *Source) FailWith(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

// FailActivityWith makes only ticks fail with err, until it is called with
// nil.
func (s *Source) FailActivityWith(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activityErr = err
}

// During runs do in the middle of every pass, after the holds - while the
// workloads are held. A test asks the daemon things mid-swap with it.
func (s *Source) During(do func(ctx context.Context)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.during = do
}

// Passes is every Pass the source has been given.
func (s *Source) Passes() []source.Pass {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.passes)
}

// Ticks is the conntrack table every activity tick was given.
func (s *Source) Ticks() [][]detect.ConntrackEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.ticks)
}

// SeenSwapping is what the status said was swapping at each hold.
func (s *Source) SeenSwapping() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.swapping)
}

func (s *Source) Name() source.Name { return s.name }

func (s *Source) Once(ctx context.Context, pass source.Pass) ([]source.Outcome, error) {
	s.mu.Lock()
	s.passes = append(s.passes, pass)
	err, holds, during := s.err, slices.Clone(s.holds), s.during
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	for _, workload := range holds {
		pass.Hold(workload)
		var swapping []string
		if s.status != nil {
			if last, ok := s.status.Last(); ok {
				swapping = last.Swapping
			}
		}
		s.mu.Lock()
		s.swapping = append(s.swapping, swapping)
		s.mu.Unlock()
	}
	if during != nil {
		during(ctx)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.outcomes), nil
}

func (s *Source) Activity(_ context.Context, conntrack []detect.ConntrackEntry) ([]source.Activity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ticks = append(s.ticks, conntrack)
	if s.err != nil {
		return nil, s.err
	}
	if s.activityErr != nil {
		return nil, s.activityErr
	}
	return slices.Clone(s.activities), nil
}

func (s *Source) List(context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	var lines []string
	for _, outcome := range s.outcomes {
		lines = append(lines, outcome.Workload)
	}
	return lines, nil
}

// Status records every status written.
type Status struct {
	mu      sync.Mutex
	written []status.Status
	cleared bool
	err     error
}

// FailWith makes Write fail with err, until it is called with nil.
func (s *Status) FailWith(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

// Written is every status written, oldest first.
func (s *Status) Written() []status.Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.written)
}

// Last is the latest status written, and false when there is none.
func (s *Status) Last() (status.Status, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.written) == 0 {
		return status.Status{}, false
	}
	return s.written[len(s.written)-1], true
}

// Cleared is whether the status was cleared after the last write.
func (s *Status) Cleared() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cleared
}

func (s *Status) Write(written status.Status) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.written = append(s.written, written)
	s.cleared = false
	return nil
}

func (s *Status) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleared = true
	return nil
}

// BootCheck says a reboot is needed for whatever reason it is given.
type BootCheck struct {
	mu     sync.Mutex
	reason string
	err    error
}

// Need says a reboot is needed, for reason; "" says it is not.
func (b *BootCheck) Need(reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reason = reason
}

// FailWith makes it unable to tell, until it is called with nil.
func (b *BootCheck) FailWith(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.err = err
}

func (b *BootCheck) Needed(context.Context) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reason, b.err
}

// Rebooter counts reboots.
type Rebooter struct {
	mu      sync.Mutex
	reboots int
	err     error
}

// FailWith makes logind refuse, until it is called with nil.
func (r *Rebooter) FailWith(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

// Reboots is how many reboots were accepted.
func (r *Rebooter) Reboots() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reboots
}

func (r *Rebooter) Reboot(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.reboots++
	return nil
}
