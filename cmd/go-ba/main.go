// go-ba service entrypoint — Phase 1: reference-data snapshot lifecycle + operational endpoints.
// The quote path itself lands in Phase 2; this process already proves the go-pe-style runtime:
// prepare-on-boot, atomic snapshot swap, polling, readiness gating, metrics.
//
// Config (env):
//
//	GO_BA_MYSQL_DSN              user:pass@tcp(host:3306)/dev1 (required)
//	GO_BA_LISTEN_ADDR            default :8091
//	GO_BA_SNAPSHOT_POLL_INTERVAL default 60s
package main

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "time/tzdata"

	_ "github.com/go-sql-driver/mysql"

	"github.com/eurosender/go-ba/internal/delegate"
	"github.com/eurosender/go-ba/internal/httpapi"
	"github.com/eurosender/go-ba/internal/lifecycle"
	"github.com/eurosender/go-ba/internal/pe"
	"github.com/eurosender/go-ba/internal/quote"
)

func main() {
	dsn := os.Getenv("GO_BA_MYSQL_DSN")
	if dsn == "" {
		log.Fatal("GO_BA_MYSQL_DSN is required")
	}
	addr := envOr("GO_BA_LISTEN_ADDR", ":8091")
	pollInterval, err := time.ParseDuration(envOr("GO_BA_SNAPSHOT_POLL_INTERVAL", "60s"))
	if err != nil {
		log.Fatalf("GO_BA_SNAPSHOT_POLL_INTERVAL: %v", err)
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		log.Fatal(err)
	}
	db.SetMaxOpenConns(4)
	db.SetConnMaxLifetime(5 * time.Minute)

	manager := lifecycle.NewManager(db, pollInterval)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go manager.Run(ctx)

	var quoteHandler, blockedRoutesHandler http.HandlerFunc
	if peURL := os.Getenv("GO_BA_PE_URL"); peURL != "" {
		svc := &quote.Service{
			Snapshot:        manager.Snapshot,
			PE:              pe.NewClient(peURL, os.Getenv("GO_BA_PE_SECRET")),
			VersionOverride: os.Getenv("GO_BA_PE_VERSION"),
			Proxy:           delegate.NewProxyFromEnv(),
		}
		if svc.Proxy != nil {
			log.Printf("delegation enabled against %s", os.Getenv("GO_BA_PHP_URL"))
		}
		quoteHandler = svc.Handle
		log.Printf("quote path enabled against %s", peURL)

		blockedRoutes := svc.NewBlockedRoutesFromService()
		blockedRoutesHandler = blockedRoutes.Handler
		if os.Getenv("GO_BA_BLOCKED_ROUTES_WARM") == "1" {
			go func() {
				// warm on boot and on every PE version change (the per-entry cache is
				// version-keyed, so a publish naturally triggers recomputation)
				var warmedVersion string
				for {
					if snap := manager.Snapshot(); snap != nil && snap.PEVersion != warmedVersion {
						blockedRoutes.Warm(ctx, 8)
						warmedVersion = snap.PEVersion
					}
					select {
					case <-ctx.Done():
						return
					case <-time.After(pollInterval):
					}
				}
			}()
		}
	}

	server := &http.Server{Addr: addr, Handler: httpapi.NewMux(manager, quoteHandler, blockedRoutesHandler)}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	log.Printf("go-ba listening on %s (poll interval %s)", addr, pollInterval)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
