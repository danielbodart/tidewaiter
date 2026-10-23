package activity

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/danielbodart/tidewaiter/internal/detect"
	"github.com/danielbodart/tidewaiter/internal/model"
)

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

const header = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"

func TestProcNetnsReadsAProcesssSocketTablesUnderItsRoot(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "42", "net", "tcp"),
		header+"   0: 00000000:6DDD 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 1 1 0 100 0 0 10 0\n")
	write(t, filepath.Join(root, "42", "net", "tcp6"),
		header+"   0: 00000000000000000000000000000000:1F90 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 2 1 0 100 0 0 10 0\n")

	ports, err := detect.PortsInState(ProcNetns{Root: root}, 42, model.TCP, detect.StateListen)
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, ports, map[int]bool{28125: true, 8080: true})
}

func TestProcNetnsCannotTellForAProcessThatIsGone(t *testing.T) {
	_, err := ProcNetns{Root: t.TempDir()}.Read(42, detect.TCP4Table)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("got %v, want not-exist", err)
	}
}

func TestProcNetnsReadsTheRealProc(t *testing.T) {
	if _, err := os.Stat("/proc/self/net/tcp"); err != nil {
		t.Skipf("no /proc here: %v", err)
	}
	if _, err := (ProcNetns{Root: "/proc"}).Read(os.Getpid(), detect.TCP4Table); err != nil {
		t.Fatal(err)
	}
}

func TestProcRootPrefersTheHostsProcWhereItIsMounted(t *testing.T) {
	host, local := filepath.Join(t.TempDir(), "host", "proc"), "/proc"
	assertEqual(t, procRoot(host, local), local)

	write(t, filepath.Join(host, "self", "net", "tcp"), header)
	assertEqual(t, procRoot(host, local), host)
}
