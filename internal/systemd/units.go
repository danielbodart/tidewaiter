package systemd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/danielbodart/tidewaiter/internal/model"
)

// The checks a unit's swap can be gated on.
//
// Docker's set minus `docker` (a unit has no HEALTHCHECK), plus `systemd`:
// the restart job's result and the unit's own ActiveState, NRestarts and
// InvocationID, which for a Type=notify unit is the program itself saying it
// is up.
const (
	CheckSystemd     = "systemd"
	CheckPortConnect = "port-connect"
	CheckPortBound   = "port-bound"
	CheckUptime      = "uptime"
)

// HealthNames is every check a unit can have, which is also the default set.
var HealthNames = []string{CheckSystemd, CheckPortConnect, CheckPortBound, CheckUptime}

// DetectorNames is the detectors a unit can have: `netio` reads Docker's
// /stats, which a unit has no equivalent of.
var DetectorNames = []string{"conntrack", "tcp", "none"}

// The defaults a unit's settings fall back to, as a container's labels do.
const (
	DefaultDetector             = "conntrack"
	DefaultIdleSamples          = 3
	DefaultHealthTimeoutSeconds = 120
	DefaultKeepGenerations      = 1
)

// Policy is a unit's resolved Tidewaiter policy, from its file in the units
// directory.
//
// The container policy's shape where the two overlap, so the shared pieces
// (decide, the detectors, the checks) read it the same way. What a unit adds
// is where its program lives: Profile is the Nix profile its ExecStart runs
// from, and Desired the store path that profile should point at.
type Policy struct {
	// The unit's full name, always with a suffix: "chandlery-bedrock.service".
	Unit string
	// The store path the profile should point at.
	Desired string
	// The Nix profile the unit's ExecStart runs from.
	Profile  string
	Detector string
	// The unit's ports, which ARE its published ports: a host unit has no
	// mapping, so HostPort == ContainerPort. Empty means the activity gate
	// cannot tell, and fails safe to busy unless the detector is `none`.
	Ports                []model.PublishedPort
	IdleSamples          int
	Health               []string
	HealthTimeoutSeconds int
	// Previous profile generations kept, for a manual rollback.
	KeepGenerations int
	// Maintain a runtime drop-in binding the profile's closure into a
	// confined unit.
	BindClosure bool
}

// unitSuffix is a unit type on the end of a name: .service, .scope, ...
var unitSuffix = regexp.MustCompile(`\.[a-z]+$`)

// ParsePolicy reads one units-directory file into a policy, collecting
// problems as data.
//
// Only `unit`, `desired` and `profile` are required; a file without them, or
// that is not a JSON object, is skipped with a warning (a nil policy) - never
// guessed at, because a guessed profile is a profile we would overwrite. Every
// other field falls back to its default with a warning, as a bad label does:
// one wrong setting must not stop the unit being managed.
//
// fileName is only for the warnings; `unit` is the identity. A `unit` written
// without a suffix is taken to be a service. Numbers are JSON numbers and
// whole: "5" or 5.5 is warned about, as the NixOS module never writes either.
func ParsePolicy(text []byte, fileName string) (*Policy, []string) {
	skip := func(reason string) (*Policy, []string) {
		return nil, []string{fmt.Sprintf("%s: %s; skipping it", fileName, reason)}
	}

	decoder := json.NewDecoder(bytes.NewReader(text))
	decoder.UseNumber()
	var parsed any
	if err := decoder.Decode(&parsed); err != nil {
		return skip(fmt.Sprintf("not valid JSON (%s)", err))
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return skip("not valid JSON (something after the object)")
	}
	fields, ok := parsed.(map[string]any)
	if !ok {
		return skip("not a JSON object")
	}

	required := map[string]string{}
	for _, name := range []string{"unit", "desired", "profile"} {
		value, _ := fields[name].(string)
		if value = strings.TrimSpace(value); value == "" {
			return skip(fmt.Sprintf("'%s' must be a non-empty string", name))
		}
		required[name] = value
	}
	for _, name := range []string{"desired", "profile"} {
		if !strings.HasPrefix(required[name], "/") {
			return skip(fmt.Sprintf("'%s' must be an absolute path, got '%s'", name, required[name]))
		}
	}

	var warnings []string
	warn := func(format string, args ...any) {
		warnings = append(warnings, fileName+": "+fmt.Sprintf(format, args...))
	}

	unit := required["unit"]
	if !unitSuffix.MatchString(unit) {
		unit += ".service"
	}

	detector := DefaultDetector
	if value, present := fields["detector"]; present {
		if name, isString := value.(string); !isString {
			warn("detector: '%s' is not a string; ignoring it", display(value))
		} else if term := strings.ToLower(strings.TrimSpace(name)); slices.Contains(DetectorNames, term) {
			detector = term
		} else {
			warn("detector: '%s' is not one of %s; using %s", name, strings.Join(DetectorNames, ", "), DefaultDetector)
		}
	}

	integer := func(name string, fallback, least int) int {
		value, present := fields[name]
		if !present {
			return fallback
		}
		number, isNumber := value.(json.Number)
		parsed, err := strconv.Atoi(string(number))
		if !isNumber || err != nil {
			warn("%s: '%s' is not a whole number; ignoring it", name, display(value))
			return fallback
		}
		if parsed < least {
			if least == 1 {
				warn("%s: '%d' must be at least 1; using %d", name, parsed, fallback)
			} else {
				warn("%s: '%d' must not be negative; using %d", name, parsed, fallback)
			}
			return fallback
		}
		return parsed
	}

	bindClosure := false
	if value, present := fields["bindClosure"]; present {
		if flag, isBool := value.(bool); isBool {
			bindClosure = flag
		} else {
			warn("bindClosure: '%s' is not true or false; using false", display(value))
		}
	}

	return &Policy{
		Unit:                 unit,
		Desired:              required["desired"],
		Profile:              required["profile"],
		Detector:             detector,
		Ports:                parsePorts(fields, warn),
		Health:               parseHealth(fields, warn),
		IdleSamples:          integer("idleSamples", DefaultIdleSamples, 1),
		HealthTimeoutSeconds: integer("healthTimeout", DefaultHealthTimeoutSeconds, 1),
		KeepGenerations:      integer("keepGenerations", DefaultKeepGenerations, 0),
		BindClosure:          bindClosure,
	}, warnings
}

// display is a JSON value as a warning quotes it: a string as itself,
// anything else as JSON.
func display(value any) string {
	switch value := value.(type) {
	case string:
		return value
	case json.Number:
		return string(value)
	}
	text, _ := json.Marshal(value)
	return string(text)
}

// port is the `tidewaiter.ports` grammar: a port, optionally /tcp or /udp.
var port = regexp.MustCompile(`(?i)^(\d{1,5})(?:/(tcp|udp))?$`)

const maxPort = 65535

// parsePorts is the unit's ports, `tidewaiter.ports` grammar, as a JSON
// array of strings or numbers.
//
// Not checked against "what is published", as a container's override is: a
// host unit publishes nothing, so the declared ports are all there is. A bare
// port means both protocols.
func parsePorts(fields map[string]any, warn func(string, ...any)) []model.PublishedPort {
	value, present := fields["ports"]
	if !present {
		return nil
	}
	list, ok := value.([]any)
	if !ok {
		warn(`ports: must be a list like ["19132/udp", "25565"]; ignoring it`)
		return nil
	}

	var ports []model.PublishedPort
	seen := map[model.PublishedPort]bool{}
	for _, raw := range list {
		term := strings.TrimSpace(display(raw))
		match := port.FindStringSubmatch(term)
		if match == nil {
			warn("ports: ignoring unrecognised port '%s'", term)
			continue
		}
		number, _ := strconv.Atoi(match[1])
		if number < 1 || number > maxPort {
			warn("ports: ignoring '%s', ports must be between 1 and %d", term, maxPort)
			continue
		}
		protocols := model.Protocols
		if match[2] != "" {
			protocols = []model.Protocol{model.Protocol(strings.ToLower(match[2]))}
		}
		for _, protocol := range protocols {
			published := model.PublishedPort{HostPort: number, ContainerPort: number, Protocol: protocol}
			if !seen[published] {
				seen[published] = true
				ports = append(ports, published)
			}
		}
	}
	return ports
}

// parseHealth is the checks to gate on, as a JSON array. Absent, empty or
// all-unknown is the default set, so a unit is never left with no gate;
// `docker` is named specifically, because it is the one a container's policy
// copied over would bring.
func parseHealth(fields map[string]any, warn func(string, ...any)) []string {
	value, present := fields["health"]
	if !present {
		return slices.Clone(HealthNames)
	}
	list, ok := value.([]any)
	if !ok {
		warn(`health: must be a list like ["systemd", "uptime"]; using the default`)
		return slices.Clone(HealthNames)
	}

	var chosen []string
	for _, raw := range list {
		term := strings.ToLower(strings.TrimSpace(display(raw)))
		switch {
		case term == "docker":
			warn("health: 'docker' needs a container's HEALTHCHECK, which a unit does not have; ignoring it")
		case !slices.Contains(HealthNames, term):
			warn("health: '%s' is not a known check (%s); ignoring it", display(raw), strings.Join(HealthNames, ", "))
		case !slices.Contains(chosen, term):
			chosen = append(chosen, term)
		}
	}
	if len(chosen) == 0 {
		return slices.Clone(HealthNames)
	}
	return chosen
}

// UnitFile is one file from the units directory, unparsed.
type UnitFile struct {
	Name string
	Text []byte
}

// UnitsDirectory is where managed units are declared: one JSON file per unit.
//
// An interface so tests hand files in from memory; the real one reads a
// directory each pass, so a unit added or dropped by a switch is picked up
// without restarting Tidewaiter.
type UnitsDirectory interface {
	Read() ([]UnitFile, error)
}

// Dir is the units directory on disk.
type Dir struct {
	Path string
}

// Read is every *.json in the directory, sorted by name; a directory that is
// not there declares nothing.
func (d Dir) Read() ([]UnitFile, error) {
	entries, err := os.ReadDir(d.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var files []UnitFile
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		text, err := os.ReadFile(filepath.Join(d.Path, entry.Name()))
		if err != nil {
			return nil, err
		}
		files = append(files, UnitFile{Name: entry.Name(), Text: text})
	}
	return files, nil
}
