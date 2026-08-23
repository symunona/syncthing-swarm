// swarmd — syncthing-swarm dashboard daemon.
// Reads swarm.yaml, polls every node's REST API, serves a folders×devices
// matrix at the configured listen address.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/symunona/syncthing-dashboard/internal/config"
	"github.com/symunona/syncthing-dashboard/internal/server"
	"github.com/symunona/syncthing-dashboard/internal/webui"
)

func main() {
	cfgPath := flag.String("config", "swarm.yaml", "path to cred store")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := server.New(cfg, webui.FS)
	srv.WatchConfig(*cfgPath) // node added to swarm.yaml -> live, no restart
	if err := srv.Run(ctx); err != nil {
		log.Printf("server: %v", err)
		os.Exit(1)
	}
}
