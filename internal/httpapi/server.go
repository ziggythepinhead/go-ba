// Package httpapi exposes go-ba's operational surface: /livez, /readyz (gated on the first
// prepared snapshot, go-pe-style) and /metrics (hand-rolled text format, same as go-pe).
package httpapi

import (
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/eurosender/go-ba/internal/lifecycle"
)

func NewMux(m *lifecycle.Manager) *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})

	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !m.Ready() {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintln(w, "snapshot not prepared")
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})

	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		prepared := 0
		snap := m.Snapshot()
		if snap != nil {
			prepared = 1
		}
		fmt.Fprintf(w, "go_ba_prepared %d\n", prepared)
		fmt.Fprintf(w, "go_ba_uptime_seconds %.0f\n", m.Uptime().Seconds())
		fmt.Fprintf(w, "go_ba_snapshot_reloads_total %d\n", m.Reloads.Load())
		fmt.Fprintf(w, "go_ba_snapshot_load_errors_total %d\n", m.LoadErrors.Load())
		// request-path MySQL ops: zero by construction in Phase 1 (no request path exists yet);
		// the counter is declared now so Phase 2 inherits the invariant and its measurement
		fmt.Fprintf(w, "go_ba_request_path_mysql_ops_total 0\n")
		if snap != nil {
			fmt.Fprintf(w, "go_ba_snapshot_age_seconds %.0f\n", time.Since(snap.LoadedAt).Seconds())
			fmt.Fprintf(w, "go_ba_snapshot_pe_version{version=%q} 1\n", snap.PEVersion)
			tables := make([]string, 0, len(snap.Rows))
			for t := range snap.Rows {
				tables = append(tables, t)
			}
			sort.Strings(tables)
			for _, t := range tables {
				fmt.Fprintf(w, "go_ba_snapshot_rows{table=%q} %d\n", t, snap.Rows[t])
			}
		}
	})

	return mux
}
