package status

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

var (
	written = time.Date(2026, 10, 23, 3, 30, 0, 0, time.UTC)
	now     = written.Add(10 * time.Second)
)

func status(edit ...func(*Status)) Status {
	s := Status{Updated: written, Busy: []string{}, Swapping: []string{}, Workloads: []Workload{}}
	for _, e := range edit {
		e(&s)
	}
	return s
}

func TestDescribeIdleSaysSoWithEachWorkloadAndHowOldTheNewsIs(t *testing.T) {
	got := Describe(status(func(s *Status) {
		s.Workloads = []Workload{
			{Workload: "bedrock.service", Source: "systemd", Settled: true},
			{Workload: "web", Source: "docker"},
		}
	}), now)
	want := []string{
		"Idle: nothing in use, nothing being swapped",
		"  bedrock.service (systemd)  idle",
		"  web (docker)  idle, not yet for long enough",
		"As of 10s ago",
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %q", got)
	}
}

func TestDescribeBusyNamesWhatIsInUse(t *testing.T) {
	got := Describe(status(func(s *Status) {
		s.Busy = []string{"bedrock.service"}
		s.Workloads = []Workload{{Workload: "bedrock.service", Source: "systemd", Busy: true}}
	}), now)
	if got[0] != "Busy: bedrock.service in use" || got[1] != "  bedrock.service (systemd)  in use" {
		t.Errorf("got %q", got)
	}
}

func TestDescribeASwapUnderWayComesFirst(t *testing.T) {
	got := Describe(status(func(s *Status) {
		s.Busy, s.Swapping = []string{"web"}, []string{"bedrock.service"}
		s.Workloads = []Workload{{Workload: "bedrock.service", Source: "systemd"}}
	}), now)
	if got[0] != "Busy: swapping bedrock.service" || got[1] != "  bedrock.service (systemd)  swapping" {
		t.Errorf("got %q", got)
	}
}

func TestDescribeWhereAnAutomaticRebootStands(t *testing.T) {
	for reboot, want := range map[Reboot]string{
		{Window: "03:00-05:00", Verdict: "not needed"}:                                               "Automatic reboot (03:00-05:00): not needed",
		{Window: "03:00-05:00", Needed: "kernel changed since boot", Verdict: "waiting: web in use"}: "Automatic reboot (03:00-05:00): needed, kernel changed since boot; waiting: web in use",
		// Not knowing is not the same as not needed.
		{Window: "03:00-05:00", Verdict: "could not tell whether a reboot is needed (x)"}: "Automatic reboot (03:00-05:00): could not tell whether a reboot is needed (x)",
	} {
		got := Describe(status(func(s *Status) { s.Reboot = &reboot }), now)
		if !slices.Contains(got, want) {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestBusyIsAnythingInUseOrSwapping(t *testing.T) {
	if IsBusy(status()) || !IsBusy(status(func(s *Status) { s.Busy = []string{"web"} })) || !IsBusy(status(func(s *Status) { s.Swapping = []string{"web"} })) {
		t.Error("wrong")
	}
}

func TestFreshUntilStaleAfterHasPassedSinceItWasWritten(t *testing.T) {
	s := status(func(s *Status) { s.StaleAfter = 60 })
	if IsStale(s, now) || IsStale(s, now.Add(50*time.Second)) || !IsStale(s, now.Add(51*time.Second)) {
		t.Error("wrong")
	}
}

func TestNeverStaleWithoutStaleAfter(t *testing.T) {
	if IsStale(status(), now.Add(24*time.Hour)) {
		t.Error("stale")
	}
}

func TestTheFileIsWrittenWholeCreatingItsDirectoryReadBackAndCleared(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "run", "status.json")
	file := File{Path: path}
	written := status(func(s *Status) { s.Busy, s.StaleAfter = []string{"web"}, 60 })

	if err := file.Write(written); err != nil {
		t.Fatal(err)
	}
	read, found, err := Read(path)
	if err != nil || !found || !reflect.DeepEqual(read, written) {
		t.Errorf("got %+v, %v, %v", read, found, err)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "run"))
	if len(entries) != 1 {
		t.Errorf("left %v", entries)
	}

	if err := file.Clear(); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := Read(path); found {
		t.Error("still there")
	}
	if err := file.Clear(); err != nil {
		t.Errorf("clearing twice: %v", err)
	}
}

// Scripts read this file, so it is the format the TypeScript Tidewaiter
// wrote: an ISO time to the millisecond, lists never null.
func TestTheFormatIsTheOneScriptsAlreadyRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	if err := (File{Path: path}).Write(Status{Updated: written, StaleAfter: 60}); err != nil {
		t.Fatal(err)
	}
	text, _ := os.ReadFile(path)
	want := `{
  "updated": "2026-10-23T03:30:00.000Z",
  "staleAfter": 60,
  "busy": [],
  "swapping": [],
  "workloads": []
}
`
	if string(text) != want {
		t.Errorf("got\n%s", text)
	}

	ts := `{"updated":"2026-10-23T03:30:00.000Z","staleAfter":60,"busy":["web"],"swapping":[],` +
		`"workloads":[{"workload":"web","source":"docker","busy":true,"settled":false}],` +
		`"reboot":{"window":"03:00-05:00","verdict":"not needed"}}`
	if err := os.WriteFile(path, []byte(ts), 0o644); err != nil {
		t.Fatal(err)
	}
	read, _, err := Read(path)
	if err != nil || !read.Updated.Equal(written) || read.Workloads[0].Source != "docker" || read.Reboot.Verdict != "not needed" {
		t.Errorf("got %+v, %v", read, err)
	}
}

func command(t *testing.T, written *Status, at time.Time) (int, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "status.json")
	if written != nil {
		if err := (File{Path: path}).Write(*written); err != nil {
			t.Fatal(err)
		}
	}
	var out strings.Builder
	code, err := Command(path, at, &out)
	if err != nil {
		t.Fatal(err)
	}
	return code, out.String()
}

func TestCommandExitsZeroWhenNothingIsInUseOrSwapping(t *testing.T) {
	s := status()
	code, out := command(t, &s, now)
	if code != Idle || !strings.HasPrefix(out, "Idle:") {
		t.Errorf("%d %q", code, out)
	}
}

func TestCommandExitsOneWhenSomethingIsInUseOrSwapping(t *testing.T) {
	for _, s := range []Status{status(func(s *Status) { s.Busy = []string{"bedrock.service"} }), status(func(s *Status) { s.Swapping = []string{"bedrock.service"} })} {
		if code, _ := command(t, &s, now); code != Busy {
			t.Errorf("got %d", code)
		}
	}
}

func TestCommandExitsTwoForAStatusTheDaemonStoppedWritingHoweverIdle(t *testing.T) {
	s := status(func(s *Status) { s.StaleAfter = 60 })
	code, out := command(t, &s, written.Add(10*time.Minute))
	if code != Unknown || !strings.Contains(out, "Stale status at") {
		t.Errorf("%d %q", code, out)
	}
}

func TestCommandBelievesAStatusWrittenWithinStaleAfter(t *testing.T) {
	s := status(func(s *Status) { s.StaleAfter, s.Busy = 60, []string{"bedrock.service"} })
	if code, _ := command(t, &s, now); code != Busy {
		t.Errorf("got %d", code)
	}
}

func TestCommandExitsTwoWhenThereIsNoStatus(t *testing.T) {
	code, out := command(t, nil, now)
	if code != Unknown || !strings.Contains(out, "No status at") {
		t.Errorf("%d %q", code, out)
	}
}
