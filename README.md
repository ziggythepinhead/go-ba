# go-ba

Go quote orchestrator for `POST /api/v2/quote` — the backend counterpart to go-pe. Design and
rationale: `~/work/ai/go-ba/ARCHITECTURE_REVIEW_2026-07-10_by_Claude.md`.

## Phase 0 (this state): corpus gate

The php backend is the oracle; the corpus is the spec. Nothing ships that doesn't reproduce the
oracle's responses over the corpus.

```bash
# record (re-record same-day before gating: responses are date-dependent)
go run ./cmd/corpus-record -oracle https://stagea.eurosender.dev -out testdata/corpus.json

# gate a candidate
go run ./cmd/corpus-gate -corpus testdata/corpus.json -target http://localhost:8091

# stability self-check (target = oracle)
go run ./cmd/corpus-gate -corpus testdata/corpus.json -target https://stagea.eurosender.dev
```

Baseline 2026-07-10: 56 cases recorded from stagea (8 routes × 5 parcel profiles × guest/zip/company
variants), self-check **56/56 GATE CLEAN** (oracle deterministic over the corpus; PePrices caching
makes repeats stable within its TTL windows).

Comparator: objects by key union, arrays by index (order is part of the contract), floats within
`-tolerance` (default 1e-9), `-ignore` takes wildcarded path prefixes (`data.x.*.y`) for fields
proven volatile — none needed so far.

## Next phases (see the architecture review §12)

1. ~~Reference-data snapshot + lifecycle~~ (done — prepare-on-boot, poller, readiness, /metrics).
2. ~~Skeleton path: decode → resolve → **one go-pe `/api/quote/services` call** → assemble → encode~~ (done).
3. Parity grind on the corpus (use `testdata/corpus-prod.json` — real prod shapes) + `GO_BA_SHADOW` tee from the php QuoteAction.
4. **Endpoint #2: `GET /api/v2/countries/blocked-routes`** — the matrix lives in the snapshot and
   answers in microseconds. Domain rule: **"no prices, no route"** — it's a pure function of the
   published PE version, so go-ba precomputes ALL combos per publish itself (~1,780 routes from
   price_engine_route × ~3 user types via go-pe bulk, async off the version poller, atomic swap;
   absent pairs = constant fully-blocked). No php blocked_routes store/cron dependency; top-client
   per-user rows lazy/proxied. Flat string-list response → trivially corpus-gateable → can ship
   BEFORE full quote parity as go-ba's first production surface.
5. Traefik route-split cutover, php path kept warm.
