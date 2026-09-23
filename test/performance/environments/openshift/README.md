# Tier-2 environment (ephemeral OpenShift)

Automation for the **tier-2** performance environment: a multi-node ephemeral
OpenShift cluster with a 5–10 GB dataset, run within a ~90-minute session budget.
Tier 2 exposes planner regressions, index bloat, and lock contention that the
tier-1 kind environment (<1 GB) is too small to surface.

Every phase accepts `--cluster kind|openshift` (default `openshift`) so the whole
pipeline can be **dry-run locally on kind** before booking OpenShift time. In kind
mode the phases fall back to smaller counts and skip OpenShift-only assumptions
(multi-node, dedicated database node); kind results are dev-only, never a tier-2
baseline.

## Phases

| Script            | Phase | Does                                                                 |
| ----------------- | ----- | ------------------------------------------------------------------- |
| `00-cluster.sh`   | 0     | Select + validate the target cluster (`--cluster kind\|openshift`). |
| `01-install.sh`   | 1     | Install Results + Postgres (dedicated node + tuning, or `BENCH_DB_URL`). |
| `02-load.sh`      | 2     | Parallel seed load via the API (or `BENCH_SEED_DUMP` restore, query-only). |
| `03-benchmark.sh` | 3     | Run store/query/mixed at `--tier tier2`; write JSON reports.        |
| `04-collect.sh`   | 4     | Validate + collect reports; print compare/promote next steps.       |

`common.sh` is sourced by every phase for `--cluster` resolution and phase-budget
timing; it is not executed directly.

## Usage

```bash
cd test/performance/environments/openshift

# Local dry run of the whole tier-2 pipeline on kind:
export BENCH_CLUSTER=kind
./00-cluster.sh && ./01-install.sh && ./02-load.sh && ./03-benchmark.sh && ./04-collect.sh

# Real tier-2 run against a booked cluster (KUBECONFIG points at it):
export BENCH_CLUSTER=openshift
./00-cluster.sh
./01-install.sh
./02-load.sh
./03-benchmark.sh
./04-collect.sh
```

## Tunables (env)

| Variable               | Purpose                                                        |
| ---------------------- | -------------------------------------------------------------- |
| `BENCH_CLUSTER`        | `kind` or `openshift` (or pass `--cluster`).                   |
| `BENCH_BUDGET_SECONDS` | Session budget for the phase-timer warnings (default 5400).    |
| `BENCH_COUNT`          | Top-level PipelineRuns to load/benchmark.                      |
| `BENCH_CONCURRENCY`    | Worker concurrency for load and benchmark.                     |
| `BENCH_NAMESPACES`     | Namespaces to spread the dataset across.                       |
| `BENCH_DURATION`       | Query/mixed run duration (seconds).                            |
| `BENCH_OUTPUT_DIR`     | Where reports are written (default `output/tier2`).            |
| `BENCH_DB_URL`         | Use an external managed Postgres instead of the in-cluster one.|
| `BENCH_SEED_DUMP`      | Restore a pg_dump snapshot as the **query-only** seed.         |

The `tier2-5-10gb` dataset size is reached by tuning `BENCH_COUNT` /
`BENCH_NAMESPACES`; capture the exact values in the committed baseline metadata.
