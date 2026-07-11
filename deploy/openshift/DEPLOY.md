# go-ba stage deploy (backend-stagea)

Same flow as go-pe's stagepe3 deploy: binary build into the namespace's imagestream, then apply
the manifest. All commands operator-run (`oc login` first).

## 0. Verify environment values (one-time)

```bash
oc get svc -n backend-stagea                      # -> confirm the php service name for GO_BA_PHP_URL
oc get svc go-pe -n pricing-engine-stagepe3       # -> confirm the go-pe service (port 80)
```

Fix `GO_BA_PHP_URL` in `go-ba.yaml` if the nginx service is not literally `nginx-be`.

## 1. Secrets (once per environment; values from the stage config, never committed)

```bash
oc create secret generic go-ba-secrets -n backend-stagea \
  --from-literal=GO_BA_MYSQL_DSN='USER:PASS@tcp(MYSQL_HOST:3306)/DBNAME' \
  --from-literal=GO_BA_PE_SECRET='STAGE_MANAGER_SECRET_KEY'
```

- DSN: the same MySQL the stagea php backend reads (reference tables only, read-only usage;
  a replica endpoint is preferable if one exists).
- `GO_BA_PE_SECRET`: stage `MANAGER_SECRET_KEY` (php `include/config.inc.php`, env-provisioned).

## 2. Build the image (binary build from the repo checkout)

```bash
cd ~/work/go-ba
oc new-build --binary --name go-ba -n backend-stagea --strategy docker   # once
oc start-build go-ba -n backend-stagea --from-dir . --follow
```

## 3. Deploy

```bash
oc apply -f deploy/openshift/go-ba.yaml -n backend-stagea
oc rollout status deploy/go-ba -n backend-stagea
```

Readiness gates on the snapshot being prepared; `GO_BA_BLOCKED_ROUTES_WARM=1` then precomputes the
blocked-routes matrix in the background (pods serve immediately; uncached pairs compute on demand,
~tens of ms against in-cluster go-pe).

## 4. Smoke (from any pod in the namespace, or via a temporary route)

```bash
oc exec deploy/go-ba -n backend-stagea -- wget -qO- localhost:8080/readyz
oc exec deploy/go-ba -n backend-stagea -- wget -qO- localhost:8080/metrics | head
# quote through go-ba vs php directly (same-plane, same clock):
oc run curl --rm -it --image=curlimages/curl -n backend-stagea --restart=Never -- \
  sh -c 'curl -s -X POST -H "Content-Type: application/json" -d "$PAYLOAD" http://go-ba/api/v2/quote'
# blocked-routes:
#   http://go-ba/api/v2/countries/blocked-routes?pickupCountryId=191&deliveryCountryId=105&userType=guest
```

The full corpus gate can run in-cluster the same way it runs on devbox: record against the stagea
php service, gate against `http://go-ba` (`cmd/corpus-record` / `cmd/corpus-gate` with
`-oracle`/`-target` pointed at the two services) — record and gate same-hour, quiet plane.

## 5. Cutover (later, ops)

Route-level split at the edge: `/api/v2/quote` + `/api/v2/countries/blocked-routes` → go-ba
service, php kept warm as the delegation target. `GO_BA_PHP_URL` must keep pointing at the php
service directly (never the public edge) — the `X-GoBa-Delegated` loop guard 508s if a delegated
request ever routes back.

## Rollback

Route flip back (cutover stage), or `oc delete -f deploy/openshift/go-ba.yaml` — go-ba is
stateless (snapshot rebuilt on boot); php is untouched throughout.
