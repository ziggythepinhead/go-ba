// Package lifecycle owns go-ba's snapshot lifecycle — go-pe's runPrepareLifecycle pattern:
// prepare on boot with retry, publish behind an atomic pointer, poll for changes, expose
// readiness only after the first successful prepare.
package lifecycle

import (
	"context"
	"database/sql"
	"log"
	"sync/atomic"
	"time"

	"github.com/eurosender/go-ba/internal/refdata"
)

type Manager struct {
	db           *sql.DB
	pollInterval time.Duration

	snapshot atomic.Pointer[refdata.Snapshot]

	Reloads    atomic.Int64
	LoadErrors atomic.Int64
	startedAt  time.Time
}

func NewManager(db *sql.DB, pollInterval time.Duration) *Manager {
	return &Manager{db: db, pollInterval: pollInterval, startedAt: time.Now()}
}

// Snapshot returns the current snapshot, or nil before the first successful prepare.
func (m *Manager) Snapshot() *refdata.Snapshot { return m.snapshot.Load() }

// Ready reports whether a snapshot has been prepared — the /readyz gate.
func (m *Manager) Ready() bool { return m.snapshot.Load() != nil }

func (m *Manager) Uptime() time.Duration { return time.Since(m.startedAt) }

// Run prepares on boot (retrying with backoff until the first success) and then polls. Blocks
// until ctx is done; the HTTP server should already be serving 503s on /readyz while this warms.
func (m *Manager) Run(ctx context.Context) {
	backoff := time.Second
	for m.snapshot.Load() == nil {
		if err := m.reload(ctx); err != nil {
			m.LoadErrors.Add(1)
			log.Printf("boot prepare failed (retrying in %s): %v", backoff, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
	}

	ticker := time.NewTicker(m.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := m.reload(ctx); err != nil {
				// keep serving the previous snapshot — reference data going stale for one
				// interval beats flapping readiness on a transient MySQL error
				m.LoadErrors.Add(1)
				log.Printf("snapshot reload failed (serving previous): %v", err)
			}
		}
	}
}

func (m *Manager) reload(ctx context.Context) error {
	loadCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	next, err := refdata.Load(loadCtx, m.db)
	if err != nil {
		return err
	}
	prev := m.snapshot.Swap(next)
	m.Reloads.Add(1)
	if prev == nil {
		log.Printf("snapshot prepared: %d tables, peVersion=%s", len(next.Rows), next.PEVersion)
		return nil
	}
	for table, n := range next.Rows {
		if prev.Rows[table] != n {
			log.Printf("snapshot reload: %s rows %d -> %d", table, prev.Rows[table], n)
		}
	}
	if prev.PEVersion != next.PEVersion {
		log.Printf("snapshot reload: peVersion %s -> %s", prev.PEVersion, next.PEVersion)
	}
	return nil
}
