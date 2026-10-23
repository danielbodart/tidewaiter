package decide

import "testing"

// inputs is an idle container with a newer image waiting, its streak already
// at the debounce; each test changes what it is about.
func inputs(edit ...func(*Inputs)) Inputs {
	in := Inputs{
		IdleSamples: 3,
		// The manifest digest the container is on now - a real digest in the
		// same namespace as Desired, so the two genuinely compare.
		Current:    "sha256:old",
		Desired:    "sha256:new",
		InUse:      false,
		IdleStreak: 3,
	}
	for _, e := range edit {
		e(&in)
	}
	return in
}

func TestKeepsAContainerAlreadyOnTheDesiredDigest(t *testing.T) {
	action := Decide(inputs(func(in *Inputs) { in.Desired = "sha256:old" }), State{}, ImageWords)
	if action.Kind != Keep {
		t.Errorf("got %v", action)
	}
}

func TestUpdatesAnIdleContainerWhenANewerImageIsAvailable(t *testing.T) {
	action := Decide(inputs(), State{}, ImageWords)
	if action.Kind != Update || action.From != "sha256:old" || action.To != "sha256:new" {
		t.Errorf("got %+v", action)
	}
	if action.Reason != "idle for 3 samples and a newer image is available" {
		t.Errorf("reason %q", action.Reason)
	}
}

func TestDefersWhenInUseHoweverLongTheIdleStreak(t *testing.T) {
	action := Decide(inputs(func(in *Inputs) { in.InUse, in.IdleStreak = true, 99 }), State{}, ImageWords)
	if action.Kind != Defer || action.Reason != ImageWords.Busy {
		t.Errorf("got %+v", action)
	}
}

func TestDefersUntilTheIdleStreakReachesTheDebounce(t *testing.T) {
	short := Decide(inputs(func(in *Inputs) { in.IdleStreak = 2 }), State{}, ImageWords)
	if short.Kind != Defer || short.Reason != "idle 2/3 samples - waiting for the debounce window" {
		t.Errorf("got %+v", short)
	}
	if got := Decide(inputs(func(in *Inputs) { in.IdleStreak = 3 }), State{}, ImageWords).Kind; got != Update {
		t.Errorf("got %v", got)
	}
}

func TestStaysPinnedToADigestAPreviousUpdateFailedOn(t *testing.T) {
	if got := Decide(inputs(), State{Pinned: "sha256:new"}, ImageWords).Kind; got != Pinned {
		t.Errorf("got %v", got)
	}
}

func TestThePinDoesNotBlockADifferentNewerDigest(t *testing.T) {
	action := Decide(inputs(func(in *Inputs) { in.Desired = "sha256:newer" }), State{Pinned: "sha256:new"}, ImageWords)
	if action.Kind != Update {
		t.Errorf("got %v", action.Kind)
	}
}

// The post-rollback state: the pull moved the local tag to the bad digest, so
// current == desired == pinned, but the container was put back on the old
// image. The pin must win over keep, or the failed update would be forgotten
// and the container treated as up to date on a bad image.
func TestThePinWinsEvenWhenTheLocalDigestEqualsIt(t *testing.T) {
	in := inputs(func(in *Inputs) { in.Desired, in.Current = "sha256:bad", "sha256:bad" })
	if got := Decide(in, State{Pinned: "sha256:bad"}, ImageWords).Kind; got != Pinned {
		t.Errorf("got %v", got)
	}
}

func TestUpdatesWhenTheImageIsNotPresentLocallyAtAll(t *testing.T) {
	action := Decide(inputs(func(in *Inputs) { in.Current = "" }), State{}, ImageWords)
	if action.Kind != Update || action.From != "" {
		t.Errorf("got %+v", action)
	}
}

func TestAUnitIsDescribedInItsOwnWords(t *testing.T) {
	if got := Decide(inputs(func(in *Inputs) { in.InUse = true }), State{}, UnitWords).Reason; got != UnitWords.Busy {
		t.Errorf("got %q", got)
	}
}
