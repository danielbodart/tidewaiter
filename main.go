// Command tidewaiter updates Docker containers and Nix-profile systemd units
// only when they are idle. Everything it does is in internal/cli; this is only
// the process around it.
package main

import (
	"context"
	"os"

	"github.com/danielbodart/tidewaiter/internal/cli"
)

func main() {
	os.Exit(cli.Main(context.Background(), &cli.Container{}))
}
