package systemd

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danielbodart/tidewaiter/internal/health"
	"github.com/danielbodart/tidewaiter/internal/model"
)

var required = map[string]any{
	"unit":    "chandlery-bedrock.service",
	"desired": "/nix/store/abc-bedrock",
	"profile": "/nix/var/nix/profiles/chandlery/bedrock",
}

func parse(t *testing.T, fields map[string]any) (*Policy, []string) {
	t.Helper()
	all := maps.Clone(required)
	maps.Copy(all, fields)
	for name, value := range all {
		if value == nil {
			delete(all, name)
		}
	}
	text, err := json.Marshal(all)
	if err != nil {
		t.Fatal(err)
	}
	return ParsePolicy(text, "bedrock.json")
}

func udpPort(port int) model.PublishedPort {
	return model.PublishedPort{HostPort: port, ContainerPort: port, Protocol: model.UDP}
}

func tcpPort(port int) model.PublishedPort {
	return model.PublishedPort{HostPort: port, ContainerPort: port, Protocol: model.TCP}
}

func TestParsePolicyReadsAFullFile(t *testing.T) {
	policy, warnings := parse(t, map[string]any{
		"ports":           []string{"19132/udp", "19133/udp"},
		"detector":        "conntrack",
		"health":          []string{"systemd", "port-bound"},
		"idleSamples":     5,
		"healthTimeout":   60,
		"keepGenerations": 2,
		"bindClosure":     true,
	})
	assertEqual(t, warnings, nil)
	assertEqual(t, policy, &Policy{
		Unit:                 "chandlery-bedrock.service",
		Desired:              "/nix/store/abc-bedrock",
		Profile:              "/nix/var/nix/profiles/chandlery/bedrock",
		Ports:                []model.PublishedPort{udpPort(19132), udpPort(19133)},
		Detector:             "conntrack",
		Health:               []string{"systemd", "port-bound"},
		IdleSamples:          5,
		HealthTimeoutSeconds: 60,
		KeepGenerations:      2,
		BindClosure:          true,
	})
}

func TestParsePolicyDefaultsEverythingButTheThreeRequiredFields(t *testing.T) {
	policy, warnings := parse(t, nil)
	assertEqual(t, warnings, nil)
	assertEqual(t, policy.Ports, nil)
	assertEqual(t, policy.Detector, "conntrack")
	assertEqual(t, policy.Health, HealthNames)
	assertEqual(t, policy.IdleSamples, 3)
	assertEqual(t, policy.HealthTimeoutSeconds, 120)
	assertEqual(t, policy.KeepGenerations, 1)
	assertEqual(t, policy.BindClosure, false)
}

func TestParsePolicyTakesABarePortToMeanBothProtocols(t *testing.T) {
	policy, _ := parse(t, map[string]any{"ports": []any{"25565", 7777}})
	assertEqual(t, policy.Ports, []model.PublishedPort{tcpPort(25565), udpPort(25565), tcpPort(7777), udpPort(7777)})
}

func TestParsePolicyTakesEachPortOnce(t *testing.T) {
	policy, _ := parse(t, map[string]any{"ports": []any{"25565/UDP", "25565", " 25565/udp "}})
	assertEqual(t, policy.Ports, []model.PublishedPort{udpPort(25565), tcpPort(25565)})
}

func TestParsePolicyWarnsAboutABadPortAndDropsItKeepingTheGoodOnes(t *testing.T) {
	policy, warnings := parse(t, map[string]any{"ports": []any{"19132/udp", "99999", "http", 1.5}})
	assertEqual(t, policy.Ports, []model.PublishedPort{udpPort(19132)})
	assertEqual(t, warnings, []string{
		"bedrock.json: ports: ignoring '99999', ports must be between 1 and 65535",
		"bedrock.json: ports: ignoring unrecognised port 'http'",
		"bedrock.json: ports: ignoring unrecognised port '1.5'",
	})
}

func TestParsePolicyWarnsAboutAndDefaultsEachBadSetting(t *testing.T) {
	policy, warnings := parse(t, map[string]any{
		"detector": "netio", "idleSamples": 0, "healthTimeout": "soon", "keepGenerations": -1, "bindClosure": "yes", "ports": "7777",
	})
	assertEqual(t, policy.Detector, "conntrack")
	assertEqual(t, policy.IdleSamples, 3)
	assertEqual(t, policy.HealthTimeoutSeconds, 120)
	assertEqual(t, policy.KeepGenerations, 1)
	assertEqual(t, policy.BindClosure, false)
	assertEqual(t, policy.Ports, nil)
	assertEqual(t, len(warnings), 6)
	assertEqual(t, warnings[0], "bedrock.json: detector: 'netio' is not one of conntrack, tcp, none; using conntrack")
	for _, want := range []string{
		"bedrock.json: idleSamples: '0' must be at least 1; using 3",
		"bedrock.json: healthTimeout: 'soon' is not a whole number; ignoring it",
		"bedrock.json: keepGenerations: '-1' must not be negative; using 1",
		"bedrock.json: bindClosure: 'yes' is not true or false; using false",
		`bedrock.json: ports: must be a list like ["19132/udp", "25565"]; ignoring it`,
	} {
		if !strings.Contains(strings.Join(warnings, "\n"), want) {
			t.Errorf("no %q in %q", want, warnings)
		}
	}
}

func TestParsePolicyTakesOnlyWholeJSONNumbers(t *testing.T) {
	policy, warnings := parse(t, map[string]any{"idleSamples": "5", "healthTimeout": 2.5})
	assertEqual(t, policy.IdleSamples, 3)
	assertEqual(t, policy.HealthTimeoutSeconds, 120)
	assertEqual(t, warnings, []string{
		"bedrock.json: idleSamples: '5' is not a whole number; ignoring it",
		"bedrock.json: healthTimeout: '2.5' is not a whole number; ignoring it",
	})
}

func TestParsePolicyReadsTheDetectorWhateverItsCase(t *testing.T) {
	policy, warnings := parse(t, map[string]any{"detector": " TCP "})
	assertEqual(t, warnings, nil)
	assertEqual(t, policy.Detector, "tcp")
	_, warnings = parse(t, map[string]any{"detector": 5})
	assertEqual(t, warnings, []string{"bedrock.json: detector: '5' is not a string; ignoring it"})
}

func TestParsePolicyRefusesDockerAsACheckAUnitCanHave(t *testing.T) {
	policy, warnings := parse(t, map[string]any{"health": []string{"docker", "uptime", "bogus"}})
	assertEqual(t, policy.Health, []string{"uptime"})
	if !strings.Contains(warnings[0], "'docker' needs a container's HEALTHCHECK") || !strings.Contains(warnings[1], "'bogus' is not a known check") {
		t.Fatal(warnings)
	}
}

func TestParsePolicyKeepsTheDefaultGateForAnEmptyOrAllUnknownHealthList(t *testing.T) {
	policy, _ := parse(t, map[string]any{"health": []string{}})
	assertEqual(t, policy.Health, HealthNames)
	policy, _ = parse(t, map[string]any{"health": []string{"docker"}})
	assertEqual(t, policy.Health, HealthNames)
	policy, warnings := parse(t, map[string]any{"health": "uptime"})
	assertEqual(t, policy.Health, HealthNames)
	assertEqual(t, len(warnings), 1)
}

func TestParsePolicyTakesAUnitNamedWithoutASuffixToBeAService(t *testing.T) {
	policy, _ := parse(t, map[string]any{"unit": "chandlery-bedrock"})
	assertEqual(t, policy.Unit, "chandlery-bedrock.service")
	policy, _ = parse(t, map[string]any{"unit": "game.scope"})
	assertEqual(t, policy.Unit, "game.scope")
}

func TestParsePolicySkipsAFileMissingARequiredField(t *testing.T) {
	for _, field := range []string{"unit", "desired", "profile"} {
		policy, warnings := parse(t, map[string]any{field: nil})
		assertEqual(t, policy, (*Policy)(nil))
		assertEqual(t, warnings, []string{"bedrock.json: '" + field + "' must be a non-empty string; skipping it"})
		policy, _ = parse(t, map[string]any{field: "  "})
		assertEqual(t, policy, (*Policy)(nil))
	}
}

func TestParsePolicySkipsARelativeDesiredOrProfile(t *testing.T) {
	policy, warnings := parse(t, map[string]any{"desired": "result"})
	assertEqual(t, policy, (*Policy)(nil))
	assertEqual(t, warnings, []string{"bedrock.json: 'desired' must be an absolute path, got 'result'; skipping it"})
	policy, _ = parse(t, map[string]any{"profile": "profiles/x"})
	assertEqual(t, policy, (*Policy)(nil))
}

func TestParsePolicySkipsWhatIsNotJSONOrNotAnObject(t *testing.T) {
	_, warnings := ParsePolicy([]byte("{ nope"), "x.json")
	if !strings.HasPrefix(warnings[0], "x.json: not valid JSON") {
		t.Fatal(warnings)
	}
	_, warnings = ParsePolicy([]byte("[1]"), "x.json")
	assertEqual(t, warnings, []string{"x.json: not a JSON object; skipping it"})
	policy, _ := ParsePolicy([]byte(`{"unit": "a", "desired": "/a", "profile": "/a"} {}`), "x.json")
	assertEqual(t, policy, (*Policy)(nil))
}

func TestDirReadsEveryJSONFileSortedAndNothingElse(t *testing.T) {
	dir := t.TempDir()
	for name, text := range map[string]string{"b.service.json": "{}", "a.service.json": "[]", "README": "not a unit"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files, err := Dir{Path: dir}.Read()
	assertEqual(t, err, nil)
	assertEqual(t, files, []UnitFile{{Name: "a.service.json", Text: []byte("[]")}, {Name: "b.service.json", Text: []byte("{}")}})
}

func TestADirThatIsNotThereDeclaresNothing(t *testing.T) {
	files, err := Dir{Path: "/nonexistent/tidewaiter/units"}.Read()
	assertEqual(t, err, nil)
	assertEqual(t, len(files), 0)
}

// The systemd check

var baseline = UnitState{Name: "game.service", LoadState: "loaded", ActiveState: "active", Result: "success", InvocationID: "aa"}

func with(change func(*UnitState)) UnitState {
	state := baseline
	change(&state)
	return state
}

func TestVerdictPassesAUnitActiveOnTheInvocationTheJobStarted(t *testing.T) {
	assertEqual(t, Verdict(JobDone, baseline, baseline), health.Passed)
	assertEqual(t, Verdict(JobDone, baseline, with(func(s *UnitState) { s.ActiveState = "reloading" })), health.Passed)
}

func TestVerdictWaitsWhileItIsStillActivating(t *testing.T) {
	assertEqual(t, Verdict(JobDone, baseline, with(func(s *UnitState) { s.ActiveState = "activating" })), health.Waiting)
}

func TestVerdictFailsAnyJobThatDidNotEndDone(t *testing.T) {
	for _, job := range []JobResult{JobCanceled, JobTimeout, JobFailed, JobDependency, JobSkipped} {
		assertEqual(t, Verdict(job, baseline, baseline), health.Failed("the restart job ended '"+string(job)+"'"))
	}
}

func TestVerdictFailsAUnitThatHasStoppedWithSystemdsReason(t *testing.T) {
	assertEqual(t, Verdict(JobDone, baseline, with(func(s *UnitState) { s.ActiveState, s.Result = "failed", "exit-code" })),
		health.Failed("the unit is failed (exit-code)"))
	assertEqual(t, Verdict(JobDone, baseline, with(func(s *UnitState) { s.ActiveState = "inactive" })),
		health.Failed("the unit is inactive"))
	assertEqual(t, Verdict(JobDone, baseline, with(func(s *UnitState) { s.ActiveState = "deactivating" })).Outcome, health.Fail)
}

func TestVerdictFailsAUnitRestartQuietlyBroughtBack(t *testing.T) {
	assertEqual(t, Verdict(JobDone, baseline, with(func(s *UnitState) { s.InvocationID = "bb" })).Outcome, health.Fail)
	assertEqual(t, Verdict(JobDone, baseline, with(func(s *UnitState) { s.NRestarts = 1 })),
		health.Failed("the unit crashed and was restarted since it came up"))
}
