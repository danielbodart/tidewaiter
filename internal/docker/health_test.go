package docker

import (
	"context"
	"testing"

	"github.com/danielbodart/tidewaiter/internal/health"
	"github.com/danielbodart/tidewaiter/internal/model"
)

func TestTheDockerCheckTrustsDockersHealthcheck(t *testing.T) {
	for status, want := range map[model.HealthStatus]health.Outcome{
		model.Healthy:   health.Pass,
		model.Unhealthy: health.Fail,
		model.Starting:  health.Inconclusive,
		// No HEALTHCHECK skips: it is not a failure to lack one.
		model.NoHealth: health.Skip,
	} {
		result := Check.Evaluate(context.Background(), health.Input{Subject: health.ContainerSubject(container("app", func(c *model.RunningContainer) { c.Health = status }))})
		if result.Outcome != want {
			t.Errorf("%s: got %v", status, result.Outcome)
		}
	}
	if Check.Name() != "docker" {
		t.Errorf("named %q", Check.Name())
	}
}

func TestAHealthEventIsReadTheSameWay(t *testing.T) {
	if Verdict(model.Healthy).Outcome != health.Pass {
		t.Error("healthy should pass")
	}
	if unhealthy := Verdict(model.Unhealthy); unhealthy.Outcome != health.Fail || unhealthy.Reason != "Docker reports the container unhealthy" {
		t.Errorf("got %+v", unhealthy)
	}
}
