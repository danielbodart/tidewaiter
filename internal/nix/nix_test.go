package nix

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

var ctx = context.Background()

func assertEqual[T any](t *testing.T, got, want T) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %#v\nwant %#v", got, want)
	}
}

func TestGenerationOfReadsTheNumberOutOfAGenerationLink(t *testing.T) {
	assertEqual(t, GenerationOf("/nix/var/nix/profiles/chandlery/bedrock", "bedrock-12-link"), 12)
	assertEqual(t, GenerationOf("/p/bedrock", "/p/bedrock-3-link"), 3)
}

func TestGenerationOfIsZeroForAnythingElse(t *testing.T) {
	for _, link := range []string{"/nix/store/abc-bedrock", "java-3-link", "bedrock-x-link", "bedrock-0-link", "bedrock--link", "bedrock-+3-link"} {
		assertEqual(t, GenerationOf("/p/bedrock", link), 0)
	}
}

func TestClosureDropInBindsTheProfilesDirectoryAndTheClosureReadOnlySortedAndOnceEach(t *testing.T) {
	assertEqual(t, ClosureDropInText("/nix/var/nix/profiles/chandlery/bedrock", []string{
		"/nix/store/bbb-bedrock-server", "/nix/store/aaa-glibc", "/nix/store/bbb-bedrock-server",
	}), strings.Join([]string{
		"# Written by tidewaiter before each restart: what the profile points at, and its closure.",
		"# Profile: /nix/var/nix/profiles/chandlery/bedrock",
		"[Service]",
		"BindReadOnlyPaths=/nix/var/nix/profiles/chandlery",
		"BindReadOnlyPaths=/nix/store/aaa-glibc",
		"BindReadOnlyPaths=/nix/store/bbb-bedrock-server",
		"",
	}, "\n"))
}

func TestDropInProfileIsWhatTheDropInSaysOrNothing(t *testing.T) {
	assertEqual(t, DropInProfile(ClosureDropInText("/p/game", nil)), "/p/game")
	assertEqual(t, DropInProfile("[Service]\n"), "")
}

const ensureProfile = "/nix/var/nix/profiles/tidewaiter/game"

// binder is a closure binder over a scratch runtime directory, with a
// nix-store that answers each path as its own closure.
func binder(t *testing.T) (NixClosureBinder, func() string) {
	t.Helper()
	dir := t.TempDir()
	run := RunnerFunc(func(_ context.Context, program string, args ...string) ([]byte, error) {
		assertEqual(t, program, "nix-store")
		assertEqual(t, args[:2], []string{"--query", "--requisites"})
		return []byte(args[2] + "\n"), nil
	})
	read := func() string {
		text, err := os.ReadFile(filepath.Join(dir, "game.service.d", ClosureDropIn))
		if err != nil {
			t.Fatal(err)
		}
		return string(text)
	}
	return NixClosureBinder{RuntimeDir: dir, Run: run}, read
}

func TestEnsureWritesADropInThatIsMissing(t *testing.T) {
	closure, read := binder(t)
	wrote, err := closure.Ensure(ctx, "game.service", ensureProfile, "/nix/store/aaaa-game-1")
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, wrote, true)
	if !strings.Contains(read(), "BindReadOnlyPaths=/nix/store/aaaa-game-1\n") {
		t.Fatal(read())
	}
	assertEqual(t, DropInProfile(read()), ensureProfile)
	leftovers, _ := filepath.Glob(filepath.Join(closure.RuntimeDir, "game.service.d", "*.tmp"))
	assertEqual(t, len(leftovers), 0)
}

func TestEnsureLeavesTheDaemonsDropInForTheSameProfileAloneWhateverItBinds(t *testing.T) {
	closure, read := binder(t)
	if err := closure.Bind(ctx, "game.service", ensureProfile, "/nix/store/bbbb-game-2"); err != nil {
		t.Fatal(err)
	}
	wrote, err := closure.Ensure(ctx, "game.service", ensureProfile, "/nix/store/aaaa-game-1")
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, wrote, false)
	if !strings.Contains(read(), "BindReadOnlyPaths=/nix/store/bbbb-game-2\n") {
		t.Fatal(read())
	}
}

func TestEnsureReplacesOneWrittenForAnotherProfile(t *testing.T) {
	closure, read := binder(t)
	if err := closure.Bind(ctx, "game.service", ensureProfile+"-old", "/nix/store/bbbb-game-2"); err != nil {
		t.Fatal(err)
	}
	wrote, err := closure.Ensure(ctx, "game.service", ensureProfile, "/nix/store/aaaa-game-1")
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, wrote, true)
	assertEqual(t, DropInProfile(read()), ensureProfile)
}

func TestBindSaysWhatNixStoreSaid(t *testing.T) {
	closure := NixClosureBinder{RuntimeDir: t.TempDir(), Run: RunnerFunc(func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("nix-store --query --requisites /nix/store/x exited with status 1: path is not valid")
	})}
	err := closure.Bind(ctx, "game.service", ensureProfile, "/nix/store/x")
	if err == nil || !strings.Contains(err.Error(), "is not valid") {
		t.Fatalf("got %v", err)
	}
}

func TestExecSaysHowAProgramFailedAndWhatItSaid(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh here")
	}
	_, err = Exec{}.Run(ctx, sh, "-c", "echo nope >&2; exit 3")
	if err == nil || !strings.Contains(err.Error(), "exited with status 3: nope") {
		t.Fatalf("got %v", err)
	}
	out, err := Exec{}.Run(ctx, sh, "-c", "echo yes")
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, string(out), "yes\n")
}

func TestReadsOfAProfileThatIsNotThereAreNothing(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "game")
	profiles := NixProfiles{}
	current, err := profiles.Current(ctx, profile)
	assertEqual(t, current, "")
	assertEqual(t, err, nil)
	generation, err := profiles.Generation(ctx, profile)
	assertEqual(t, generation, 0)
	assertEqual(t, err, nil)
}

func TestAProfileLinkedToSomethingThatIsNotAGenerationIsAnError(t *testing.T) {
	dir := t.TempDir()
	profile := filepath.Join(dir, "game")
	if err := os.Symlink("/nix/store/abc-game", profile); err != nil {
		t.Fatal(err)
	}
	if _, err := (NixProfiles{}).Generation(ctx, profile); err == nil || !strings.Contains(err.Error(), "not a profile generation") {
		t.Fatalf("got %v", err)
	}
}

// storePaths is store paths that are certainly there, for a real profile to
// point at: whatever sh, ls and env resolve to on a Nix host.
func storePaths() []string {
	var paths []string
	for _, command := range []string{"sh", "ls", "env"} {
		found, err := exec.LookPath(command)
		if err != nil {
			continue
		}
		real, err := filepath.EvalSymlinks(found)
		if err != nil || !strings.HasPrefix(real, "/nix/store/") {
			continue
		}
		path := "/nix/store/" + strings.SplitN(strings.TrimPrefix(real, "/nix/store/"), "/", 2)[0]
		if !slices.Contains(paths, path) {
			paths = append(paths, path)
		}
	}
	return paths
}

// realNix is two store paths, or a skip where there is no nix-env with a
// daemon to talk to - the Nix build's sandbox has neither.
func realNix(t *testing.T) []string {
	t.Helper()
	if _, err := exec.LookPath("nix-env"); err != nil {
		t.Skip("no nix-env here")
	}
	if _, err := os.Stat("/nix/var/nix/daemon-socket/socket"); err != nil {
		t.Skip("no Nix daemon here")
	}
	paths := storePaths()
	if len(paths) < 2 {
		t.Skip("fewer than two store paths to point a profile at")
	}
	return paths
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, entry := range entries {
		found = append(found, entry.Name())
	}
	slices.Sort(found)
	return found
}

func TestNixProfilesAgainstNixEnv(t *testing.T) {
	paths := realNix(t)
	a, b := paths[0], paths[1]
	dir := t.TempDir()
	profile := filepath.Join(dir, "not-yet", "game")
	profiles := NixProfiles{Run: Exec{}}

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	current, err := profiles.Current(ctx, profile)
	must(err)
	assertEqual(t, current, "")

	must(profiles.Set(ctx, profile, a))
	must(profiles.Set(ctx, profile, b))
	must(profiles.Set(ctx, profile, a))
	generation, err := profiles.Generation(ctx, profile)
	must(err)
	assertEqual(t, generation, 3)
	current, err = profiles.Current(ctx, profile)
	must(err)
	assertEqual(t, current, a)
	target, err := profiles.Target(ctx, profile, 2)
	must(err)
	assertEqual(t, target, b)

	must(profiles.SwitchGeneration(ctx, profile, 2))
	must(profiles.DeleteGeneration(ctx, profile, 3))
	current, err = profiles.Current(ctx, profile)
	must(err)
	assertEqual(t, current, b)
	assertEqual(t, names(t, filepath.Join(dir, "not-yet")), []string{"game", "game-1-link", "game-2-link"})

	// nix-env numbers it one past the highest left: 3 again.
	must(profiles.Set(ctx, profile, a))
	must(profiles.Prune(ctx, profile, 1))
	assertEqual(t, names(t, filepath.Join(dir, "not-yet")), []string{"game", "game-2-link", "game-3-link"})
}

func TestNixProfilesFailureSaysWhatNixEnvSaid(t *testing.T) {
	paths := realNix(t)
	profile := filepath.Join(t.TempDir(), "game")
	profiles := NixProfiles{Run: Exec{}}
	if err := profiles.Set(ctx, profile, paths[0]); err != nil {
		t.Fatal(err)
	}
	err := profiles.SwitchGeneration(ctx, profile, 9)
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("got %v", err)
	}
}

func TestNixClosureBinderAgainstNixStore(t *testing.T) {
	paths := realNix(t)
	dir := t.TempDir()
	if err := (NixClosureBinder{RuntimeDir: dir, Run: Exec{}}).Bind(ctx, "game.service", "/nix/var/nix/profiles/tidewaiter/game", paths[0]); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "game.service.d", ClosureDropIn))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "BindReadOnlyPaths=/nix/var/nix/profiles/tidewaiter\n") || !strings.Contains(text, "BindReadOnlyPaths="+paths[0]+"\n") {
		t.Fatal(text)
	}
	store := 0
	for line := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(line, "BindReadOnlyPaths=/nix/store/") {
			store++
		}
	}
	if store < 2 {
		t.Fatalf("only %d store paths bound:\n%s", store, text)
	}
}
