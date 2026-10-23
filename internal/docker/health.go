package docker

import (
	"context"

	"github.com/danielbodart/tidewaiter/internal/health"
	"github.com/danielbodart/tidewaiter/internal/model"
)

// Check trusts Docker's own HEALTHCHECK verdict, when the image defines one.
//
// Authoritative about the app's internal self-check: healthy passes,
// unhealthy fails, starting is inconclusive, and none - no HEALTHCHECK -
// SKIPS. Lacking one is not a failure, it just means this check has nothing
// to say; failing it would veto every healthcheck-less container under the
// combined gate.
var Check health.Check = dockerCheck{}

type dockerCheck struct{}

func (dockerCheck) Name() string { return "docker" }

func (dockerCheck) Evaluate(_ context.Context, in health.Input) health.Result {
	return Verdict(in.Subject.Health)
}

// Verdict is what Docker's health says, as a check result - for the poll,
// and for a health_status event, which settles the gate without waiting for
// the next one.
func Verdict(status model.HealthStatus) health.Result {
	switch status {
	case model.Healthy:
		return health.Passed
	case model.Unhealthy:
		return health.Failed("Docker reports the container unhealthy")
	case model.Starting:
		return health.Waiting
	}
	return health.Skipped
}
