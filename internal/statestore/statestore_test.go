package statestore

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestAFileThatIsNotThereIsNoMemories(t *testing.T) {
	memories, err := File{Path: filepath.Join(t.TempDir(), "systemd.json")}.Load()
	if err != nil || len(memories) != 0 || memories == nil {
		t.Errorf("got %v, %v", memories, err)
	}
}

func TestSavesWholeCreatingTheDirectoryAndLoadsBack(t *testing.T) {
	dir := t.TempDir()
	store := File{Path: filepath.Join(dir, "state", FileName)}
	good, from := 7, 6
	memories := Memories{
		"bedrock.service": {Pinned: "/nix/store/bad", Good: &good},
		"hytale.service": {Failing: "/nix/store/x", SwapFailures: 2, Swapping: &SwapRecord{
			From: &from, FromTarget: "/nix/store/old", To: "/nix/store/new", InvocationID: "abc",
		}},
	}
	if err := store.Save(memories); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil || !reflect.DeepEqual(loaded, memories) {
		t.Errorf("got %+v, %v", loaded, err)
	}
	// Nothing left behind from the rename.
	entries, _ := os.ReadDir(filepath.Join(dir, "state"))
	if len(entries) != 1 {
		t.Errorf("left %v", entries)
	}
}

// An existing install's pins must survive the upgrade from the TypeScript
// Tidewaiter, so its file is read as it wrote it.
func TestReadsWhatTheTypeScriptTidewaiterWrote(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	written := `{
  "chandlery-bedrock.service": {
    "good": 12,
    "pinned": "/nix/store/aaa-bedrock-2",
    "swapping": {
      "from": 12,
      "fromTarget": "/nix/store/bbb-bedrock-1",
      "to": "/nix/store/aaa-bedrock-2",
      "invocationId": "0f1e2d"
    }
  },
  "other.service": { "failing": "/nix/store/ccc", "swapFailures": 1 }
}
`
	if err := os.WriteFile(path, []byte(written), 0o644); err != nil {
		t.Fatal(err)
	}
	memories, err := File{Path: path}.Load()
	if err != nil {
		t.Fatal(err)
	}
	bedrock := memories["chandlery-bedrock.service"]
	if bedrock.Pinned != "/nix/store/aaa-bedrock-2" || *bedrock.Good != 12 || *bedrock.Swapping.From != 12 ||
		bedrock.Swapping.FromTarget != "/nix/store/bbb-bedrock-1" || bedrock.Swapping.InvocationID != "0f1e2d" {
		t.Errorf("got %+v %+v", bedrock, bedrock.Swapping)
	}
	if other := memories["other.service"]; other.Failing != "/nix/store/ccc" || other.SwapFailures != 1 || other.Good != nil {
		t.Errorf("got %+v", other)
	}
}

// Silently forgetting pins is exactly what this file exists to stop.
func TestAFileThatCannotBeReadIsAnErrorNotNoMemories(t *testing.T) {
	for _, text := range []string{"{not json", "[]", "null", ""} {
		path := filepath.Join(t.TempDir(), FileName)
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := (File{Path: path}).Load(); err == nil {
			t.Errorf("%q loaded", text)
		}
	}
}
