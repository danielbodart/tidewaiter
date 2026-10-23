package activity

import (
	"os"
	"path/filepath"
	"strconv"

	"github.com/danielbodart/tidewaiter/internal/detect"
)

// ProcNetns reads a process's netns-scoped socket tables from a real /proc.
//
// Root is normally /proc, but in the deployed container it is /host/proc -
// the host's /proc bind-mounted read-only. That mount is what lets the daemon
// reach another container's /proc/<pid>/net/* at all: sharing the host network
// namespace does not share the host PID namespace, so without either
// `pid: host` or this mount the daemon cannot see other containers' PIDs. The
// read-only mount is the smaller grant, so it is what the compose uses.
type ProcNetns struct{ Root string }

func (p ProcNetns) Read(pid int, table detect.Table) (string, error) {
	text, err := os.ReadFile(filepath.Join(p.Root, strconv.Itoa(pid), "net", string(table)))
	return string(text), err
}

// ProcRoot is where to read PIDs' /proc from: the host's /proc bind-mounted
// at /host/proc if it is there, else the local one. Resolved once at startup,
// so a missing mount degrades to /proc rather than being an error.
func ProcRoot() string { return procRoot("/host/proc", "/proc") }

func procRoot(host, local string) string {
	if _, err := os.Stat(filepath.Join(host, "self", "net", "tcp")); err == nil {
		return host
	}
	return local
}
