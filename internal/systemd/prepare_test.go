package systemd

import (
	"errors"
	"strings"
	"testing"

	"github.com/danielbodart/gokit/logx/logtest"
)

type world struct {
	journal  *journal
	units    *fakeUnits
	profiles *fakeProfiles
	closure  *fakeClosure
	systemd  *fakeSystemd
	log      *logtest.Logger
}

func newWorld() *world {
	journal := &journal{}
	return &world{
		journal:  journal,
		units:    newFakeUnits(),
		profiles: newFakeProfiles(journal),
		closure:  newFakeClosure(journal),
		systemd:  newFakeSystemd(journal),
		log:      &logtest.Logger{},
	}
}

func (w *world) prepare(t *testing.T) Prepared {
	t.Helper()
	prepared, err := Prepare(ctx, PrepareDeps{Units: w.units, Profiles: w.profiles, Closure: w.closure, Systemd: w.systemd, Log: w.log})
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

func TestPrepareCreatesAMissingProfileOnTheDesiredVersion(t *testing.T) {
	w := newWorld()
	w.units.declare("game.service", map[string]any{"desired": v1, "profile": profile})
	assertEqual(t, w.prepare(t), Prepared{Seeded: []string{"game.service"}})
	current, _ := w.profiles.Current(ctx, profile)
	assertEqual(t, current, v1)
	// Nothing to reload when no drop-in was written.
	assertEqual(t, w.journal.lines, []string{"set " + v1})
}

func TestPrepareNeverMovesAProfileThatExists(t *testing.T) {
	w := newWorld()
	w.profiles.seed(profile, v1)
	w.units.declare("game.service", map[string]any{"desired": v2, "profile": profile})
	assertEqual(t, w.prepare(t).Seeded, nil)
	current, _ := w.profiles.Current(ctx, profile)
	assertEqual(t, current, v1)
	assertEqual(t, w.journal.lines, nil)
}

func TestPrepareBindsTheClosureOfWhatTheProfilePointsAtNowNotOfDesiredThenReloadsOnce(t *testing.T) {
	w := newWorld()
	w.profiles.seed(profile, v1)
	w.units.declare("game.service", map[string]any{"desired": v2, "profile": profile, "bindClosure": true})
	w.units.declare("other.service", map[string]any{"desired": v2, "profile": profile + "-other", "bindClosure": true})
	assertEqual(t, w.prepare(t).Bound, []string{"game.service", "other.service"})
	assertEqual(t, w.journal.lines, []string{"bind " + v1, "set " + v2, "bind " + v2, "daemon-reload"})
}

func TestPrepareLeavesADropInThatIsAlreadyThereToTheDaemonAndDoesNotReloadForNothing(t *testing.T) {
	w := newWorld()
	w.profiles.seed(profile, v1)
	w.closure.dropIns["game.service"] = dropIn{profile, v1}
	w.units.declare("game.service", map[string]any{"desired": v2, "profile": profile, "bindClosure": true})
	assertEqual(t, w.prepare(t).Bound, nil)
	assertEqual(t, w.journal.lines, nil)
}

func TestPrepareRunBetweenTheDaemonsBindAndItsSetLeavesTheDropInNamingDesired(t *testing.T) {
	w := newWorld()
	w.profiles.seed(profile, v1)
	w.units.declare("game.service", map[string]any{"desired": v2, "profile": profile, "bindClosure": true})
	w.closure.Bind(ctx, "game.service", profile, v2) // the daemon, mid-swap
	w.prepare(t)                                     // reads the profile while it still says v1
	w.profiles.Set(ctx, profile, v2)                 // the daemon carries on
	assertEqual(t, w.closure.dropIns["game.service"].target, v2)
}

func TestPrepareRewritesADropInBoundForAProfileTheDeclarationNoLongerNames(t *testing.T) {
	w := newWorld()
	w.profiles.seed(profile, v1)
	w.closure.dropIns["game.service"] = dropIn{profile + "-old", v2}
	w.units.declare("game.service", map[string]any{"desired": v2, "profile": profile, "bindClosure": true})
	assertEqual(t, w.prepare(t).Bound, []string{"game.service"})
	assertEqual(t, w.closure.dropIns["game.service"], dropIn{profile, v1})
}

func TestPrepareCarriesOnPastAUnitThatFailsAndReportsIt(t *testing.T) {
	w := newWorld()
	w.profiles.setFails = errors.New("cannot connect to the Nix daemon")
	w.units.declare("a.service", map[string]any{"desired": v1, "profile": profile + "-a"})
	w.units.declare("b.service", map[string]any{"desired": v1, "profile": profile + "-b"})
	prepared := w.prepare(t)
	assertEqual(t, prepared.Failed, []string{"a.service"})
	assertEqual(t, prepared.Seeded, []string{"b.service"})
	if !w.log.ErrorContains("a.service: could not prepare it (cannot connect") {
		t.Fatal(w.log.Text())
	}
}

func TestPrepareFailsEveryUnitAReloadThatFailsWasFor(t *testing.T) {
	w := newWorld()
	w.systemd.reloadFails = errors.New("no bus")
	w.units.declare("game.service", map[string]any{"desired": v1, "profile": profile, "bindClosure": true})
	assertEqual(t, w.prepare(t).Failed, []string{"game.service"})
	if !w.log.ErrorContains("Error: wrote the closure drop-ins but could not reload systemd (no bus)") {
		t.Fatal(w.log.Text())
	}
}

func TestPrepareSkipsAFileItCannotUseWithItsWarning(t *testing.T) {
	w := newWorld()
	w.units.files["broken.json"] = "{"
	assertEqual(t, w.prepare(t), Prepared{})
	if !strings.Contains(w.log.Lines()[0], "broken.json: not valid JSON") {
		t.Fatal(w.log.Text())
	}
}

type unreadable struct{}

func (unreadable) Read() ([]UnitFile, error) { return nil, errors.New("permission denied") }

func TestPrepareFailsWhenTheUnitsDirectoryCannotBeRead(t *testing.T) {
	w := newWorld()
	_, err := Prepare(ctx, PrepareDeps{Units: unreadable{}, Profiles: w.profiles, Closure: w.closure, Systemd: w.systemd, Log: w.log})
	if err == nil {
		t.Fatal("no error")
	}
}
