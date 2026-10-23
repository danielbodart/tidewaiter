package reboot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// at is a time of day on some day.
func at(hours, minutes int) time.Time {
	return time.Date(2026, 10, 23, hours, minutes, 0, 0, time.Local)
}

func window(t *testing.T, text string) Window {
	t.Helper()
	w, err := ParseWindow(text)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestParseWindowReadsMinutesSinceMidnight(t *testing.T) {
	for text, want := range map[string]Window{
		"03:00-05:00": {180, 300},
		"23:30-01:15": {1410, 75},
		"00:00-24:00": {0, 1440},
	} {
		if got := window(t, text); got != want {
			t.Errorf("%s: got %v", text, got)
		}
	}
}

func TestParseWindowRefusesWhatIsNotAWindow(t *testing.T) {
	for _, text := range []string{"3:00-5:00", "03:00", "03:00-05:00-06:00", "25:00-01:00", "03:60-04:00", "24:00-01:00", "", "late"} {
		if _, err := ParseWindow(text); err == nil || !strings.Contains(err.Error(), "not a window") {
			t.Errorf("%q: got %v", text, err)
		}
	}
}

func TestParseWindowRefusesAnEmptyWindowRatherThanGuessingAllDay(t *testing.T) {
	_, err := ParseWindow("03:00-03:00")
	if err == nil || !strings.Contains(err.Error(), "empty window") || !strings.Contains(err.Error(), "00:00-24:00") {
		t.Errorf("got %v", err)
	}
}

func TestContainsFromTheStartUpToButNotIncludingTheEnd(t *testing.T) {
	w := window(t, "03:00-05:00")
	for when, want := range map[time.Time]bool{at(2, 59): false, at(3, 0): true, at(4, 59): true, at(5, 0): false, at(15, 0): false} {
		if w.Contains(when) != want {
			t.Errorf("%v: want %v", when, want)
		}
	}
}

func TestAWindowAcrossMidnightIsTheEndOfOneDayAndTheStartOfTheNext(t *testing.T) {
	w := window(t, "23:00-01:00")
	for when, want := range map[time.Time]bool{at(22, 59): false, at(23, 0): true, at(0, 0): true, at(0, 59): true, at(1, 0): false, at(12, 0): false} {
		if w.Contains(when) != want {
			t.Errorf("%v: want %v", when, want)
		}
	}
}

func TestTheWholeDay(t *testing.T) {
	w := window(t, "00:00-24:00")
	if !w.Contains(at(0, 0)) || !w.Contains(at(23, 59)) {
		t.Error("not the whole day")
	}
}

// systems lays two systems out as the store does: directories of links into
// "store" paths, with the booted system and the profile links to them.
// kernel names the installed system's kernel.
func systems(t *testing.T, kernel string, initrd bool) (NixOS, string) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"linux-1", "linux-2", "initrd-1", "modules-1"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := func(target, name string) {
		if err := os.Symlink(target, name); err != nil {
			t.Fatal(err)
		}
	}
	system := func(path, kernel string) {
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
		link(filepath.Join(dir, kernel), filepath.Join(path, "kernel"))
		if initrd {
			link(filepath.Join(dir, "initrd-1"), filepath.Join(path, "initrd"))
		}
		link(filepath.Join(dir, "modules-1"), filepath.Join(path, "kernel-modules"))
	}
	system(filepath.Join(dir, "system-1"), "linux-1")
	system(filepath.Join(dir, "system-2"), kernel)
	link(filepath.Join(dir, "system-1"), filepath.Join(dir, "booted"))
	link(filepath.Join(dir, "system-2"), filepath.Join(dir, "current"))
	return NixOS{Booted: filepath.Join(dir, "booted"), Current: filepath.Join(dir, "current")}, dir
}

func needed(t *testing.T, check NixOS) string {
	t.Helper()
	reason, err := check.Needed(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return reason
}

func TestNothingNeededWhenTheInstalledSystemBootsTheSameKernelInitrdAndModules(t *testing.T) {
	check, _ := systems(t, "linux-1", true)
	if got := needed(t, check); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestNeededWhenTheKernelChangedSayingWhatChanged(t *testing.T) {
	check, _ := systems(t, "linux-2", true)
	if got := needed(t, check); got != "kernel changed since boot" {
		t.Errorf("got %q", got)
	}
}

func TestASystemWithNoInitrdOnEitherSideIsNotAChange(t *testing.T) {
	check, _ := systems(t, "linux-1", false)
	if got := needed(t, check); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestCannotTellWhenTheInstalledSystemIsNotThere(t *testing.T) {
	check, dir := systems(t, "linux-1", true)
	if err := os.Remove(filepath.Join(dir, "current")); err != nil {
		t.Fatal(err)
	}
	_, err := check.Needed(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cannot read") || !strings.Contains(err.Error(), "current") {
		t.Errorf("got %v", err)
	}
}

func TestDecide(t *testing.T) {
	ready := Inputs{Needed: "kernel changed since boot", InWindow: true}
	edit := func(e func(*Inputs)) Inputs {
		in := ready
		e(&in)
		return in
	}
	for _, c := range []struct {
		name string
		in   Inputs
		want Verdict
	}{
		{"reboots when needed, in the window, everything settled", ready, Verdict{Kind: Reboot}},
		{"not needed outranks everything", edit(func(in *Inputs) { in.Needed, in.Busy = "", []string{"web"} }), Verdict{Kind: NotNeeded}},
		{"the window first", edit(func(in *Inputs) { in.InWindow, in.Busy = false, []string{"web"} }), Verdict{Wait, "waiting for the window"}},
		{"then a swap", edit(func(in *Inputs) { in.Swapping, in.Busy = []string{"a.service"}, []string{"web"} }), Verdict{Wait, "waiting: swapping a.service"}},
		{"then players, sorted", edit(func(in *Inputs) { in.Busy, in.Unsettled = []string{"web", "a.service"}, []string{"b"} }), Verdict{Wait, "waiting: a.service, web in use"}},
		{"then the debounce", edit(func(in *Inputs) { in.Unsettled = []string{"b"} }), Verdict{Wait, "waiting: b not idle for long enough yet"}},
	} {
		if got := Decide(c.in); got != c.want {
			t.Errorf("%s: got %+v", c.name, got)
		}
	}
}
