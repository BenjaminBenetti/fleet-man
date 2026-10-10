package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/BenjaminBenetti/fleet-man/internal/cli"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeshim"
)

func main() {
	// Started as `docker` or `docker-compose` through a kubeshim directory:
	// the devcontainer CLI is calling the docker shim the Kubernetes backend
	// set up for it.
	if kubeshim.Invoked(os.Args[0]) {
		os.Exit(kubeshim.Main())
	}

	// A root context cancelled on SIGINT/SIGTERM. The fleet server (run via the
	// hidden `fleet server` subcommand) watches cmd.Context() for this so it can
	// shut down gracefully; ordinary commands are unaffected.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := cli.NewRootCmd().ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}
