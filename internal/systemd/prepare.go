package systemd

import (
	"context"
	"fmt"

	"github.com/danielbodart/gokit/logx"
	"github.com/danielbodart/tidewaiter/internal/nix"
)

// Reloader is systemd's daemon-reload, all prepare needs of it.
type Reloader interface {
	DaemonReload(ctx context.Context) error
}

// PrepareDeps is what prepare talks to.
type PrepareDeps struct {
	Units    UnitsDirectory
	Profiles nix.Profiles
	Closure  nix.ClosureBinder
	Systemd  Reloader
	// Silent when nil.
	Log logx.Logger
}

// Prepared is what prepare did, by unit.
type Prepared struct {
	// Units whose profile did not exist and now points at their desired
	// version.
	Seeded []string
	// Units whose closure drop-in was written; one already there is left
	// alone.
	Bound []string
	// Units that could not be prepared; their errors are logged.
	Failed []string
}

// Prepare makes every declared unit startable, before anything starts it.
//
// Two things a unit running from a profile needs that nothing else provides:
//
//   - A profile. A unit declared for the first time has none, and its
//     ExecStart would fail. It is set to desired straight away: nothing is
//     running on it, so there is nobody to wait for. An existing profile is
//     never touched here - moving it is the daemon's job, when the unit is
//     idle.
//   - Its closure drop-in, for a confined unit (bindClosure). The drop-in
//     lives in /run, so it is gone after every reboot, and is written for
//     whatever the profile points at now - which after a rollback is not what
//     the unit's own definition names. Only written when missing, or bound for
//     a different profile: once one exists the daemon is its only writer,
//     since a prepare racing a swap could otherwise put the old closure back
//     between the daemon's bind and its restart.
//
// Then one daemon-reload, so the drop-ins are in force before the units
// start. Run by the NixOS module's tidewaiter-prepare.service, ordered before
// every managed unit and re-run on every switch that changes a declaration.
// One unit failing does not stop the rest; an error only when the units
// directory could not be read at all.
func Prepare(ctx context.Context, deps PrepareDeps) (Prepared, error) {
	if deps.Log == nil {
		deps.Log = logx.Discard
	}
	var prepared Prepared
	files, err := deps.Units.Read()
	if err != nil {
		return prepared, err
	}
	done := map[string]bool{}
	for _, file := range files {
		policy, warnings := ParsePolicy(file.Text, file.Name)
		for _, warning := range warnings {
			deps.Log.Log(warning)
		}
		if policy == nil || done[policy.Unit] {
			continue
		}
		done[policy.Unit] = true

		seeded, bound, err := prepareUnit(ctx, deps, policy)
		if seeded {
			prepared.Seeded = append(prepared.Seeded, policy.Unit)
		}
		if bound {
			prepared.Bound = append(prepared.Bound, policy.Unit)
		}
		if err != nil {
			prepared.Failed = append(prepared.Failed, policy.Unit)
			deps.Log.Error(fmt.Sprintf("%s: could not prepare it (%s)", policy.Unit, err))
		}
	}

	if len(prepared.Bound) > 0 {
		if err := deps.Systemd.DaemonReload(ctx); err != nil {
			deps.Log.Error(fmt.Sprintf("Error: wrote the closure drop-ins but could not reload systemd (%s)", err))
			prepared.Failed = append(prepared.Failed, prepared.Bound...)
		}
	}
	return prepared, nil
}

func prepareUnit(ctx context.Context, deps PrepareDeps, policy *Policy) (seeded, bound bool, err error) {
	current, err := deps.Profiles.Current(ctx, policy.Profile)
	if err != nil {
		return false, false, err
	}
	if current == "" {
		if err := deps.Profiles.Set(ctx, policy.Profile, policy.Desired); err != nil {
			return false, false, err
		}
		current, seeded = policy.Desired, true
		deps.Log.Log(fmt.Sprintf("%s: created %s on %s", policy.Unit, policy.Profile, policy.Desired))
	}
	if policy.BindClosure {
		bound, err = deps.Closure.Ensure(ctx, policy.Unit, policy.Profile, current)
	}
	return seeded, bound, err
}
