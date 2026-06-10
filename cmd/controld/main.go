// Command controld is the control plane daemon: a REST API + dashboard +
// Prometheus metrics for the replication fleet. It tracks the server inventory,
// replication jobs, and per-sync history, and computes RPO/lag.
//
// State lives in a SQLite database (pure-Go driver, no cgo). Protect the API
// with a bearer token via -token or the CONTROL_TOKEN environment variable.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tiny125/vm-replication/internal/controlplane"
	"github.com/tiny125/vm-replication/internal/store"
)

func main() {
	var (
		listen = flag.String("listen", ":8088", "HTTP listen address")
		dbPath = flag.String("db", "controld.db", "SQLite database path")
		token  = flag.String("token", os.Getenv("CONTROL_TOKEN"), "bearer token for API/metrics (default $CONTROL_TOKEN)")
	)
	flag.Parse()

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("controld: open store: %v", err)
	}
	defer st.Close()

	if *token == "" {
		log.Printf("WARNING: no -token / CONTROL_TOKEN set; API is unauthenticated (dev only)")
	}

	srv := controlplane.New(st, *token)
	httpSrv := &http.Server{
		Addr:              *listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("controld listening on %s (db=%s, dashboard=/, metrics=/metrics)", *listen, *dbPath)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("controld: serve: %v", err)
		}
	}()

	// Graceful shutdown on SIGINT/SIGTERM.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Printf("controld shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
}
