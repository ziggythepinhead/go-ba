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

## Architecture: native fast path + detect-and-delegate

go-ba serves the guest quote fast path natively (snapshot + one PE bulk call, php-parity per the
specs in `~/work/ai/go-ba/specs/`) and **delegates whole requests to the php backend** whenever a
request needs an unported feature or the native path fails — go-ba never improvises an answer.
Delegation reasons (metric `go_ba_delegated_total{reason}`, each disable-able via
`GO_BA_DELEGATE_DISABLE` csv): `auth` (Authorization/x-api-key), `coupon`, `vanftl` (van/truck
parcels, selected 9/10/14/15, routeDistance), `courier-pin` (courierId), `source` (not
website/app), `value` (declared shipment/parcel values → courier insurances), `fedex-addressed`
(fully-addressed + FedEx-priced: php runs live FedEx availability/IPE), `error` (catch-all,
incl. panics). Non-EUR currency is NATIVE (converted blocks ported).

Endpoints:
- `POST /api/v2/quote` — native or delegated per the predicate above.
- `GET /api/v2/countries/blocked-routes` — native: automated half = "no prices, no route" (11 PE
  probes per (pair, userType), cached per PE version; optional full warm via
  `GO_BA_BLOCKED_ROUTES_WARM=1`), simplified half = `enabled_simplified_routes` snapshot config.
  Authenticated calls (top clients) delegate to php. Verified 35/35 (pairs × userTypes)
  compute-vs-compute against local php.

Env: `GO_BA_MYSQL_DSN`, `GO_BA_PE_URL`, `GO_BA_PE_SECRET`, `GO_BA_PE_VERSION` (dev pin),
`GO_BA_PHP_URL` (+`GO_BA_PHP_HOST_HEADER` for devbox nginx, `GO_BA_PHP_TIMEOUT`),
`GO_BA_DELEGATE_DISABLE`, `GO_BA_BLOCKED_ROUTES_WARM`, `GO_BA_LISTEN_ADDR`,
`GO_BA_SNAPSHOT_POLL_INTERVAL`.

## Next phases (see the architecture review §12)

1. ~~Reference-data snapshot + lifecycle~~ (done).
2. ~~Skeleton path~~ (done).
3. ~~Parity grind~~ (done — php-code-first port, 317/320 fresh same-plane gate with delegation,
   residuals = transient local-franken engine compiles that pass on replay).
4. ~~Endpoint #2 blocked-routes~~ (done — see above).
5. Deploy artifacts (Dockerfile + manifest) → stage deploy → cutover with php kept warm.
   Corpus rule of thumb: record AND gate same-day, same courier-cutoff window (14:00 flips
   pickup-date math); franken must be warm (fresh restarts return empty/dummy prices until
   per-courier engines compile).
