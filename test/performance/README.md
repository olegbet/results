# Tekton Results — Performance Benchmark Framework

A repeatable benchmark for measuring **store** (write/ingest) and **query**
(read/list) performance of Tekton Results — separately and in parallel — always
starting from an identical, versioned seed dataset. It drives the **real deployed
API** only (gRPC for writes, REST/gRPC for reads); it never touches the database
directly.

Use it to prove improvement and catch regressions across schema changes (metadata
columns, label tables, indexes) and new code paths.

```
generator/     deterministic PipelineRun/TaskRun generator + golden DatasetDefinition
metrics/       latency percentiles, error-by-code counters, throughput (importable)
report/        JSON report schema + metadata capture (importable)
benchmark/     cobra "bench" CLI + store/query/mixed/load drivers (package main)
datasets/      dataset specs (see seed-small.md)
environments/  kind cluster + install automation (local & external DB)
kustomize/     benchmark deployment overlays with API client rate limits
```

## Prerequisites

- A running Tekton Results deployment. The benchmark reaches it over the
  address in `<API_SERVER_ADDR>` (default `https://localhost:8080`).
- A client certificate and a service account token with access to the
  Results API. The install scripts under
  [`environments/kind/`](../test/performance/environments/kind/) print their
  paths.
- Go >= 1.26.

You do not need database credentials. The benchmark only speaks to the API.


## Quick start (tier-1 kind, local DB)

```bash
# 1. Cluster + install with fixed Postgres tuning
./test/performance/environments/kind/00-kind-up.sh
./test/performance/environments/kind/01-install-localdb.sh
```

Both benchmark installers share the settings in
[`api-client-tuning/kustomization.yaml`](kustomize/api-client-tuning/kustomization.yaml).
Edit `K8S_QPS`, `K8S_BURST`, and `AUTH_DISABLE` directly in that file, then rerun
the appropriate installer to apply the changes. These values are API container
environment variables that override its config-file defaults.

The default values are `K8S_QPS=500`, `K8S_BURST=1000`, and `AUTH_DISABLE=false`.
To bypass the API's Kubernetes authentication and RBAC checks for a benchmark,
change the `AUTH_DISABLE` entry to `value: "true"`. TLS remains enabled; use the
usual benchmark certificate and token arguments.

The local-DB overlay extends the e2e deployment; the
[`external-db` overlay](kustomize/external-db/) deploys without in-cluster Postgres.
To inspect the manifests before installing:

```bash
kubectl kustomize test/performance/kustomize
kubectl kustomize test/performance/kustomize/external-db
```

Check if the api server address, certificate and token paths 
environment variables (should have been exposed by the 
previously executed 01-install-localdb.sh script):

```bash
export SSL_CERT_PATH=/tmp/tekton-results/ssl
export SA_TOKEN_PATH=/tmp/tekton-results/tokens
export API_SERVER_ADDR=https://localhost:8080
```
Load the versioned seed dataset through the API and verify its row counts
against the golden definition:

```bash
# 2. Load + verify the seed dataset through the API
go run ./test/performance/benchmark load --verify \
--cert "${SSL_CERT_PATH}/tekton-results-cert.pem" \
--token "${SA_TOKEN_PATH}/all-namespaces-admin-access"

# 3. Benchmark — reports are printed to stdout and saved under --output (created
#    on demand); omit --output to save /tmp/<timestamp>-<commit>.json instead.
go run ./test/performance/benchmark store  \
--cert "${SSL_CERT_PATH}/tekton-results-cert.pem" \
--token "${SA_TOKEN_PATH}/all-namespaces-admin-access" \
--output test/performance/output/store.json

go run ./test/performance/benchmark query  \
--cert "${SSL_CERT_PATH}/tekton-results-cert.pem" \
--token "${SA_TOKEN_PATH}/all-namespaces-admin-access" \
--output test/performance/output/query.json --transport both

go run ./test/performance/benchmark mixed  \
--cert "${SSL_CERT_PATH}/tekton-results-cert.pem" \
--token "${SA_TOKEN_PATH}/all-namespaces-admin-access" \
--concurrency 8 --read-ratio 3 --write-ratio 1 \
--output test/performance/output/mixed.json --duration 60
```

## Subcommands

| Command   | What it measures                                                        |
| --------- | ---------------------------------------------------------------------- |
| `load`    | Benchmarks the gRPC loading throughput of the seed dataset via the API; `--verify` checks row counts against the golden definition. |
| `store`   | Write/ingest — benchmarks the watcher lifecycle (CreateResult → CreateRecord(pending) → 1–3 UpdateRecord → UpdateResult → 2–15 child records) over the **live** UID range. |
| `query`   | Read/list — a fixed CEL query mix (by status, recent window, by type, by label) against the **seed** range, paginated. `--transport grpc\|rest\|both`. |
| `mixed`   | Writers (live range) and readers (seed range) in parallel; sized by `--read-ratio`/`--write-ratio`. |
| `dataset` | Emits the golden `DatasetDefinition` JSON — no cluster required. |

`load`, `store`, `query`, and `mixed` print the JSON benchmark report to stdout
and save the same report to the selected file. `--output` chooses the file path;
when omitted, the file defaults to `/tmp/<timestamp>-<commit>.json`. Stdout
contains the report JSON, so it can be redirected or piped to tools such as `jq`.

When `load` or `store` uses `--output report.json`, it also writes
`report.sendlog.json` with the instance UIDs, record names, and operation status
codes for ingest verification. The load report and send log are written before
`--verify` runs, so they remain available if verification fails. No sidecar is
written when `--output` is omitted.

Every `load` report includes `metrics.seed_load` with `total_instances`,
`failed_instances`, and `error_rate_percent`.
An instance comprises one PipelineRun and its child TaskRuns; any error while
writing its Result, PipelineRun, or children counts that instance once.
`AlreadyExists` responses tolerated by the loader do not count as failed
instances. The summary describes write outcomes, not an actual missing-row count.
Write errors do not fail the command; `--verify` still fails if the stored row
counts do not match the golden definition. See [Investigating seed-load errors](#investigating-seed-load-errors)
for how to find the underlying error in API and Postgres logs.

## Common flags

Defaults come from the same environment variables the e2e suite uses.

| Flag              | Default                              | Env               |
| ----------------- | ------------------------------------ | ----------------- |
| `--server-addr`   | `https://localhost:8080`             | `API_SERVER_ADDR` |
| `--server-name`   | `tekton-results-api-service…svc…`    | `API_SERVER_NAME` |
| `--cert`          | —                                    | `SSL_CERT_PATH`   |
| `--token`         | —                                    | `SA_TOKEN_PATH`   |
| `--concurrency`   | `8`                                  |                   |
| `--count`         | `1000` (count-bounded runs)          |                   |
| `--duration`      | `0` (count-bounded)                 |                   |
| `--seed`          | `42`                                 |                   |
| `--namespaces`    | `50`                                 |                   |
| `--child-min/max` | `2` / `15`                           |                   |
| `--db-type`    | `local`                              | `BENCH_DB_TYPE`   |
| `--output`        | `/tmp/<timestamp>-<commit>.json`      |                   |

Without `--duration`, `query` attempts `--count` complete paginated queries in
total across all workers (default `1000`), then writes its report. Each page
request is measured separately, so the report's operation count can exceed
`--count`. With `--transport both`, successful queries alternate between gRPC and
REST; all pages of a query use the same transport. A positive `--duration`
repeats queries for that many seconds instead of applying the query count limit.
In `mixed` mode, zero duration stops readers after all writers finish the live
dataset; a positive duration keeps the timed reader workload.

### Mixed read/write ratios

The `mixed` subcommand divides the `--concurrency` worker budget in number of workers
between readers and writers using these flags:

| Flag | Default | Meaning |
| ---- | ------- | ------- |
| `--read-ratio` | `3` | Reader share of the worker budget. |
| `--write-ratio` | `1` | Writer share of the worker budget. |

With `--concurrency 8`, the default 3:1 ratio starts **6 readers and 2 writers**,
as shown in the quick-start example. For a write-heavy run, use
`--read-ratio 1 --write-ratio 3` to start **2 readers and 6 writers**.

These ratios control worker allocation; they do not guarantee the same ratio of
completed API requests. Writer counts are rounded down, with remaining workers
assigned to readers, and at least one worker of each type is always kept.
`mixed` requires `--concurrency` of at least `2`.

## Dataset determinism

The generator is seeded (`math/rand/v2` PCG per index) and uses a fixed absolute
time window — never `time.Now()` — so a given `(seed, count, namespaces)` always
produces byte-identical objects and a stable `content_hash`. Seed data and live
(store/mixed) data draw UIDs from **disjoint** ranges so writes never collide with
loaded reads. See [`datasets/seed-small.md`](datasets/seed-small.md) for the
tier-1 spec and golden fingerprint.

Templates are anonymized real PipelineRun/TaskRun manifests dropped into
[`generator/templates/`](generator/templates/) per the contract documented there;
a sample ships so the framework builds and tests run before real manifests arrive.
Each template may carry a weighted set of child TaskRun skeletons (a `taskruns/`
directory plus `childSkeletonWeights`), so generated PipelineRuns reproduce a
realistic child-record size spread rather than repeating one identical TaskRun.

## Report schema

Every run emits a JSON `Report` (`report.SchemaVersion`) — the comparison input
for later runs:

```jsonc
{
  "schema_version": "1",
  "meta": {
    "git_commit": "…", "git_dirty": false,
    "dataset_version": "seed-small-v1", "dataset_hash": "…",
    "tier": "tier1", "mode": "load",
    "started_at": "…", "duration_ms": 1234,
    "api_server_addr": "…",
    "db_type": "local", "server_image": "…"
  },
  "config": { "count": 1000, "concurrency": 8, "transport": "grpc", "seed": 42, … },
  "metrics": {
    "throughput_per_sec": 812.3,
    "total_ops": 5000, "total_errors": 0,
    "seed_load": {
      "total_instances": 1000, "failed_instances": 0, "error_rate_percent": 0
    },
    "by_op": {
      "create_record": { "count": 1000, "errors": 0, "error_codes": {},
        "p50_ms": 3.1, "p90_ms": 7.4, "p99_ms": 19.0,
        "min_ms": 1.2, "max_ms": 41.0, "mean_ms": 4.0 }
    }
  }
}
```

Percentiles are exact (nearest-rank over sorted samples); errors are classified by
gRPC status code. Map keys serialize in stable order. `metrics.seed_load` is
present only in `load` reports. Its percentage is
`100 * failed_instances / total_instances`, or zero when no instances were
attempted. Per-operation error counts and codes remain in `metrics.by_op`.
Those counters record raw API statuses, so tolerated `AlreadyExists` responses
can appear there even when `failed_instances` is zero.

### Investigating seed-load errors

Start with the instance summary and the operations with nonzero error counts:

```bash
# Replace this with the path of your load report.
report_path=/tmp/20261022-120000-abcdef01.json
jq '{seed_load: .metrics.seed_load,
     failed_operations: (.metrics.by_op | with_entries(select(.value.errors > 0)))}' "$report_path"
```

The report contains aggregate counts and gRPC status codes. For the underlying
server or database error, collect logs covering the run's start time. These
selectors and container names match the bundled performance deployment:

```bash
run_start=$(jq -r '.meta.started_at' "$report_path")

kubectl logs -n tekton-pipelines \
  -l app.kubernetes.io/name=tekton-results-api -c api \
  --since-time="$run_start" --timestamps --prefix --tail=-1 > api.log

kubectl logs -n tekton-pipelines \
  -l app.kubernetes.io/name=tekton-results-postgres -c postgres \
  --since-time="$run_start" --timestamps --prefix --tail=-1 > postgres.log
```

Inspect `api.log` for request failures, timeouts, and database errors. Inspect
`postgres.log` for `ERROR`, `FATAL`, deadlocks, connection-limit errors, or failed
statements around the same time. Use the report's `meta.duration_ms` to identify
the run's time window, and the instance UIDs/resource names in the send log, when
available, to correlate entries. Authentication, validation, or transport errors
can occur before a database request, so they may have no matching Postgres entry.

If a container restarted, request its previous logs with `kubectl logs --previous`
using that pod and container name. For the external database overlay, use the
database's actual namespace, pod selector, and container name; for a managed
database, inspect its provider's database logs.

### What the measurements mean

The benchmark only ever calls the deployed API, so operation metrics are **end-to-end,
client-observed** measurements of API operations (`create_result`,
`create_record`, `update_record`, `create_child`, the list queries, …). It is a
**proxy for database performance**: the API server is a thin layer over Postgres,
so with the dataset held constant (same `dataset_hash`) a change in these numbers
between two runs is the change in the underlying store.

- **`throughput_per_sec`** — total API operations completed divided by the run's
  wall-clock window (`meta.duration_ms`). The headline rate; higher is better.
- **`total_ops` / `total_errors`** — operation and failure counts across every op.
- **`by_op`** — the same breakdown per operation, because a write instantiates
  several distinct calls with very different cost. `count` is the number of samples
  the percentiles are computed from; `errors`/`error_codes` classify failures by
  gRPC status code.

**Latency percentiles** describe the *distribution* of per-call durations, not an
average — latency is skewed, and a few slow calls that a mean would hide are
exactly what a storage regression looks like. Sort all samples for an operation
fastest→slowest:

- **`p50_ms`** (median) — half of calls were this fast or faster; the typical case.
- **`p90_ms`** — 90% were this fast or faster; only the slowest 10% were worse.
- **`p99_ms`** — 99% were this fast or faster; the slow **tail**. Regressions
  (e.g. a missing index) often move p99 long before they touch p50, which is why
  the pass/fail gate below is on p99, not the mean.
- **`min_ms` / `max_ms` / `mean_ms`** — best case, worst single sample, and
  arithmetic average. `mean` is included for reference only; prefer the
  percentiles when comparing runs.

Percentiles stabilize with sample count — trust `p99` at the thousands-of-samples
scale the seed dataset produces, not at a few dozen (where `p99` is just the single
slowest call). To attribute a regression to a specific SQL statement or query plan
you still need server-side DB tooling (`pg_stat_statements`, `EXPLAIN ANALYZE`);
this benchmark is black-box and tells you *that* something regressed, not *which*
query.

## External database

To benchmark against a managed/external Postgres instead of the in-cluster one:

```bash
export BENCH_DB_URL="postgres://user:pass@host:5432/results?sslmode=require"
./test/performance/environments/kind/02-install-externaldb.sh
# The installer cannot export into this shell; set the report label here:
export BENCH_DB_TYPE=external
```

This rewires only configuration (`DB_*` config + credentials secret) — no code or
image change. Exporting `BENCH_DB_TYPE=external` (or passing
`--db-type=external`) labels reports with `db_type=external`. The benchmark
is unchanged because it only talks to the API.

## Comparing runs

Reports are self-describing (git commit, dataset hash, tier, DB backend). To
compare two runs, diff `metrics.by_op[*].p99_ms` and `throughput_per_sec` for the
same `mode` and `dataset_hash`. Guideline variance for a pass/fail gate is ±10% on
p99.

## Tests

```bash
go test ./test/performance/...
```

Covers generator determinism and distributions, UID non-overlap, metrics
percentile math, report round-trip, and benchmark workload wiring.

## Smoke tests

The CI smoke run stands up local-db kind, loads `seed-small`, then runs `store|query|mixed` capped
at 1k records and asserts each emits valid JSON.

```bash
./test/performance/environments/kind/03-smoke.sh
```

Currently it can be executed manually, but later will be integrated to CI workflows (TBD).

## Multiple benchmark users

Measured `load`, `store`, `query`, and `mixed` commands accept `--token-pool FILE`.
Provide a private schema-1 manifest with stable public labels and one token file per client:

```json
{
  "schema_version": "1",
  "population_id": "cache-users",
  "clients": [
    {
      "id": "user-0000",
      "token_file": "tokens/user-0000",
      "identity": {
        "username": "system:serviceaccount:benchmark-users:user-0000",
        "uid": "recorded-account-uid",
        "provenance": "verified",
        "evidence_ref": "identity-receipt.json"
      }
    }
  ]
}
```

Token paths resolve against the manifest directory. Keep manifests/token files private
(mode 0600 under a 0700 directory), preferably outside the repository; the ignored
`test/performance/credentials/` directory is also available. Raw tokens belong only in
their token files. Tokens are read once, normalized, checked for duplicates and held
immutable for the run. Input file changes do not renew pool credentials. Known actual
expiry can be recorded as `expires_at` in RFC3339; expired credentials and credentials
known to expire before a timed workload ends are rejected. Check actual expiry across
multi-run experiments yourself, including TTL waits; unknown expiry is not proof of validity.

Use `declared` provenance when an identity mapping is unverified. `provisioned` and
`verified` require an evidence reference, and provisioned accounts require their UID.
The harness records these declarations without contacting the identity provider. Decoding
a JWT does not verify its identity. Multiple token strings for one username count as one
user; conflicting UID mappings or any declared mapping cannot certify supported distinct
users. Only the aggregate pool fingerprint is public; token values and individual token
hashes are excluded from reports.

```bash
bench query --server-addr "$API_SERVER_ADDR" --cert "$SSL_CERT_FILE" \
  --token-pool /private/cache-users/pool.json --count 1000 --concurrency 8 \
  --output test/performance/output/users-8.json
bench query --server-addr "$API_SERVER_ADDR" --cert "$SSL_CERT_FILE" \
  --token-pool /private/cache-users/pool.json --count 1000 --concurrency 1000 \
  --output test/performance/output/users-1000.json
```

Supply 1,000 distinct identities/tokens in the manifest for this comparison. `--count`
is global logical work for the run. Pool size does not multiply the count or establish
1,000 network connections. Selection uses the logical work index modulo pool size;
timed readers use a shared dispatch sequence independent of worker speed. Mixed readers
have their own sequence, while writers retain their exclusive live dataset indices.
All pages of a query and all calls of one ingestion lifecycle retain the selected user.
The existing shared TLS transports carry different concurrent identities.

Reports add pool/provenance/coverage information and per-client counters, including unused
clients. Client operation/error totals reconcile with aggregate metrics; dispatch, API
attempts, successful responses, and logical completion remain distinct. Existing latency
samples and raw status-code counts retain their meanings. Partial work cannot establish
full user coverage. Changing only credentials does not alter generated datasets or hashes.

Explicit `--token` and `--token-pool` conflict. `SA_TOKEN_FILE` is ignored when a pool is
selected. Omitting the pool retains the existing single-token workflow. The `dataset`
command remains independent of authentication and rejects pool flags.

Pool-mode `load --verify` uses the first manifest member outside measurement. Give that
member records/list permission in every seed namespace; the command does not substitute
an administrator. Reports and requested send logs are written before verification. Report,
stdout, and send-log outputs are attempted independently; a write failure returns nonzero,
preserves successful outputs, skips optional verification, and closes clients. On successful
file/stdout writes their JSON bytes are identical.

### Controlled cache and permission queries

Use `query --access-cases cases.json --dataset-count 1000 --count 8000
--transport both` to keep the seed definition fixed while repeating requests. Each
logical operation keeps its selected credential through all pages. For N clients
and K cases, `N*K` operations cover one transport; `2*N*K` cover both. Short runs
and cancellation report incomplete coverage. A mismatch fails the command after
preserving the report; expected rejections remain in raw error counts.

```json
{
  "schema_version": "1",
  "cases": [
    {"id":"list", "operation":"list-results", "parent":"ns-00",
     "expected":"allow", "expected_by_client":{"user-0001":"deny"}},
    {"id":"get", "operation":"get-record",
     "name":"ns-00/results/RESULT/records/RECORD", "expected":"allow"}
  ]
}
```

Supported operations are `list-results`, `list-records`, `get-result`, and
`get-record`. List cases accept `filter`; get targets must exist. Verify fixture
existence with an authorized identity outside measurement. A 404 is an unexpected
error. A successful page in a negative case is an unexpected grant, even if a
later page would reject access. `deny` and `invalid` accept only gRPC
Unauthenticated/PermissionDenied or HTTP 401/403, and retain their separate intended
labels. Overrides can name only pool client IDs (or `single` without a pool).
Intentionally invalid pool entries use `"identity":null` and
`"intentionally_invalid":true`; every resolved case for them must expect `invalid`.
They count as clients, not distinct users.

A rejection alone does not prove a cacheable negative decision. For denied users,
verify successful TokenReview authentication and SAR `Allowed=false` with empty
`EvaluationError`, using the exact user, groups/extra, namespace, resource and verb.
For invalid credentials, verify TokenReview `Authenticated=false` with empty
`Status.Error`. Perform these checks outside measurement and retain only non-secret
identity/access attributes and decision/error-present flags. Never retain raw
review requests, tokens, or error text. Account for inherited grants; profile
`none` does not remove them. First establish each fixture with caching disabled,
vary one access attribute at a time, then repeat isolated warm/expiry runs. A
missing or ambiguous fixture leaves its cache conclusion unverified.

### Automatic metric snapshots and TTL comparisons

Add `--metrics-config metrics.json` independently to load/store/query/mixed,
including existing single-token commands. The collector takes one bounded request
per source before workload timing and one after workers finish, before load
verification. Failed endpoints preserve workload results and the counterpart
snapshot. Collection availability, delta validity, response validation and cache
assessment are separate report fields. Missing values remain null/unavailable;
a measured zero remains zero.

Pin the same API pod through a separate loopback tunnel:

```bash
kubectl --context "$BENCH_CONTEXT" -n tekton-pipelines \
  port-forward "pod/$BENCH_POD" 19090:9090 --address=127.0.0.1
```

Restart the tunnel and update `instance_id` after each rollout. The Results metrics
listener on port 9090 is separate from workload authentication; verify that its
requests do not trigger reviews or warm the cache. Protected proxies require
independently verified traffic exclusion. Never guess-subtract collector activity.

```json
{
  "schema_version":"1",
  "sources":[{
    "id":"results-api", "url":"http://127.0.0.1:19090/metrics",
    "instance_id":"REPLACE_WITH_POD_UID", "exclusive_traffic":false,
    "timeout":"5s",
    "selectors":[
      {"id":"cpu", "metric":"process_cpu_seconds_total", "type":"counter",
       "labels":{}, "signal":"supporting"},
      {"id":"memory", "metric":"process_resident_memory_bytes", "type":"gauge",
       "labels":{}, "signal":"supporting"}
    ]
  }]
}
```

Set `exclusive_traffic:true` and `scope_evidence_ref` only with checked isolation
evidence. Selectors require exact full label maps. Remote endpoints require HTTPS;
optional `ca_file`, `server_name`, and `bearer_token_file` configure separate
collector trust/credentials, with paths relative to the metrics manifest. No
workload-token or proxy inheritance, redirects, or compressed responses are used.
Limits are four sources, 32 selectors per source, 30 seconds per request (default
five), 4 MiB and 10,000 series per response. Only selected scalar counters/gauges
are retained; raw scrapes, URLs and private paths are omitted from reports.

Counter deltas require stable observed `process_start_time_seconds`, monotonic
samples and verified declared isolation. Gauges retain endpoint values. Genuine
compatible hit/miss counters alone yield hit ratios with nonzero denominators.
PR [#1529](https://github.com/tektoncd/results/pull/1529), reviewed revision
`2b443c443b314d95f5acc14cfdba7532babf049a`, has no established direct cache metric
names: missing cache signals stay unavailable. Existing process/gRPC metrics are
supporting evidence and cannot certify cache behavior. See the
[metrics contract](../../specs/001-multiuser-cache-benchmark/contracts/metrics.md).

Follow the [controlled quickstart](./QUICKSTART_MULTIUSER.md)
for `--run-context` examples, verified seed loading and immutable environment
receipts. Keep dataset hash, pool/case fingerprints, count, transport, valid tokens,
image, routing, effective database configuration, and client/API/database resource
capacity fixed. Recheck controls before every phase. Pin one process and keep the
working set below cache capacity. Record `environment.configuration_ref` separately
from declared/verified provenance; the benchmark validates references without
fetching receipts or changing server settings.

Positive TTL governs successful authentication and allowed authorization; negative
TTL governs unauthenticated and denied decisions. Use fresh processes for two
experiments: short-positive/long-negative, then long-positive/short-negative. In
each experiment, separately bracket homogeneous allowed, authenticated-denied, and
(if definitive evidence exists) invalid cohorts. Record disabled/cold/warm and
`after-positive-expiry`/`after-negative-expiry` phases. Warm and expiry contexts
reference both the original cold report and the immediate prior report.

For original cold measurement [S,E], finish warm access before
`S + shortTTL - margin`; start short-expiry access after `E + shortTTL + margin`
and finish before `S + longTTL - margin`. Include automatic capture overhead in
wall-time planning. Increase TTL separation or reduce the functional population
if these inequalities do not fit. Use a separate 1,000-user scale run; do not infer
an expiry failure from an already expired warm entry.

### Review evidence and operator assessment

Use the [Metadata-only audit procedure](./CACHE_EVIDENCE.md)
on a disposable cluster. Filter the verified Results service-account caller,
`create` TokenReview/SAR events and `ResponseComplete`, deduplicate `auditID`, and
preserve line/inode/time bookmarks and full API process receipts (pod UID,
container ID, restart count, running start). Compare receipts within and across
phases. Require nonzero disabled/cold calibration from the same source before
accepting a warm zero. Missing events, review errors, changed process/control
receipts, unrelated traffic or collector contamination prevent acceptance.

The following Bash/jq example assembles a deliberately **unverified** assessment
from immutable files. `COUNTS` and `WINDOW` are sanitized outputs of that audit
procedure; `CONTROLS` is the operator's checked controls record. Missing review
counts stay null. Set prior/cold references appropriately before invoking it:

```bash
set -euo pipefail
: "${RUN_REPORT:?}" "${COUNTS:?}" "${WINDOW:?}" "${CONTROLS:?}"
BENCH_REPORT_SHA=$(sha256sum "$RUN_REPORT" | cut -d ' ' -f 1)
BENCH_EVIDENCE_SHA=$(sha256sum "$COUNTS" | cut -d ' ' -f 1)
BENCH_ENV_RECEIPT=$(jq -er '.scenario.environment.configuration_ref' "$RUN_REPORT")
BENCH_ENV_SHA=$(sha256sum "$BENCH_ENV_RECEIPT" | cut -d ' ' -f 1)
BENCH_PRIOR_SHA=''
BENCH_COLD_SHA=''
if [[ -n ${PRIOR_REPORT:-} ]]; then
  BENCH_PRIOR_SHA=$(sha256sum "$PRIOR_REPORT" | cut -d ' ' -f 1)
fi
if [[ -n ${COLD_REPORT:-} ]]; then
  BENCH_COLD_SHA=$(sha256sum "$COLD_REPORT" | cut -d ' ' -f 1)
fi
jq -n --slurpfile report "$RUN_REPORT" --slurpfile counts "$COUNTS" \
  --slurpfile window "$WINDOW" --slurpfile controls "$CONTROLS" \
  --arg report_sha "$BENCH_REPORT_SHA" --arg prior_sha "$BENCH_PRIOR_SHA" \
  --arg cold_sha "$BENCH_COLD_SHA" --arg evidence_ref "$COUNTS" \
  --arg evidence_sha "$BENCH_EVIDENCE_SHA" --arg env_ref "$BENCH_ENV_RECEIPT" \
  --arg env_sha "$BENCH_ENV_SHA" '{
    schema_version:"1",scenario_id:$report[0].scenario.scenario_id,
    phase:$report[0].scenario.phase,run_id:$report[0].meta.run_id,
    report_sha256:$report_sha,
    prior_report_sha256:(if $prior_sha=="" then null else $prior_sha end),
    cold_report_sha256:(if $report[0].scenario.phase=="cold" then $report_sha
      elif $cold_sha=="" then null else $cold_sha end),
    evidence:{kind:"audit_metadata",source_ref:$evidence_ref,sha256:$evidence_sha,
      start:$window[0].start,end:$window[0].end,
      caller:"system:serviceaccount:tekton-pipelines:api",
      tokenreviews:($counts[0].tokenreviews // null),
      subjectaccessreviews:($counts[0].subjectaccessreviews // null),
      failed_reviews:($counts[0].failed_reviews // null),
      complete:false,counter_reset:null,
      unavailable_reason:"operator verification required"},
    controls:($controls[0]+{environment_configuration_ref:$env_ref,
      environment_configuration_sha256:$env_sha}),
    status:"unverified",reasons:["Verify scope, calibration, timing and controls"]
  }' > "$RUN_REPORT.cache-assessment.json"
```

Resolve a relative environment-receipt reference against its original context
file's directory before running this example. Check all hashes and required
controls before an operator records passed or failed; never edit the measured
report. Retain failed response diagnostics even when cache evidence is unavailable.
Conclusions are specific to each cohort and TTL experiment.

For embedded metric evidence, use `evidence.kind:"counter_delta"` for review
activity or `"cache_counters"` for genuine direct signals; `source_ref` identifies
the immutable report and `sha256` equals `report_sha256`. Bind
`metric_source_id` and exact `selector_ids` to that report's selected samples.
Carry validated deltas/availability into nullable review counts or `cache_signals`;
never substitute gRPC counts. Verify process continuity, compatible labels, complete
capture, isolation and collector exclusion. Keep unavailable/invalid comparisons
unverified; complete contradicting evidence can justify failed.

### Optional service-account preparation

Existing pools require no preparation permissions. To prepare a small disposable
population, choose an explicit context and existing namespaces:

```bash
bench users prepare --context "$BENCH_CONTEXT" --namespace benchmark-users \
  --access-namespaces ns-00 --profile read --count 8 \
  --token-duration 24h --output-dir "$BENCH_WORK/small-users"
bench query --token-pool "$BENCH_WORK/small-users/pool.json" \
  --count 16 --concurrency 8 --output "$BENCH_WORK/small-query.json"
bench users cleanup --context "$BENCH_CONTEXT" \
  --state "$BENCH_WORK/small-users/ownership.json"
bench users cleanup --context "$BENCH_CONTEXT" \
  --state "$BENCH_WORK/small-users/ownership.json"
```

Supply the normal workload endpoint/TLS flags to the query. Use `--count 1000`
and a different new output directory to prepare the scale population; request
concurrency separately, including 1,000. Optional `--kubeconfig` does not modify
current context. The operator needs service-account create/get/delete, token
issuance, and Role/RoleBinding create/get/delete plus permission to grant/bind the
selected rules. `read` grants get/list on Results results/records; `read-write`
adds create/update. `none` requires omission of `--access-namespaces` and creates
no grants; inherited permissions still apply. No cluster-wide objects are created.

Requested token lifetime is advisory; `pool.json` records returned expiry.
Directories are 0700 and files 0600. Keep them private and outside shared artifacts.
An atomically persisted inventory precedes cluster mutation, recording intended
names and ownership markers, then returned UIDs and actual expiries. A failed or
interrupted preparation leaves partial state, exits nonzero, and publishes no
ready pool. Preserve `ownership.json` and run cleanup to recover; do not rerun
preparation into the existing directory. Cleanup checks explicit context, API
endpoint, namespace UIDs, ownership markers and object UIDs, with UID-precondition
deletes. A lost create response can be recovered through the intended name and
marker. Replaced/conflicting objects survive and cause partial cleanup. Only
inventoried credentials/pool are removed; namespaces, Results data, external pools
and unrelated resources/files remain. A non-secret cleaned receipt supports
repeat cleanup.

No PR image currently exists. Follow [Build the cache PR image locally](../../specs/001-multiuser-cache-benchmark/quickstart.md#build-the-cache-pr-image-locally)
to build revision `2b443c443b314d95f5acc14cfdba7532babf049a` in a separate
checkout with ko, load it into Kind without a registry push, set the API deployment
to that image with `IfNotPresent`, and retain the revision/image receipt. The
ordinary installer builds this worktree and does not install the cache PR by itself.
