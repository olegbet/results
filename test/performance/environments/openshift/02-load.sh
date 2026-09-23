#!/usr/bin/env bash
# Copyright 2026 The Tekton Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# Phase 2 — load the tier-2 seed dataset (target 5-10 GB) through the API.
#
# By default this loads via the real API path at high concurrency (the honest,
# code-exercising path). For a fast reset between back-to-back runs you may restore
# a previously captured pg_dump snapshot instead by setting BENCH_SEED_DUMP — this
# bypasses the API entirely, so it is valid ONLY to seed the read (query) dataset,
# never as input to a store benchmark.
#
# Tunables (env): BENCH_COUNT, BENCH_CONCURRENCY, BENCH_NAMESPACES, BENCH_SEED_DUMP.

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(git rev-parse --show-toplevel)"
HARNESS="${ROOT}/test/performance/harness"
NAMESPACE="${BENCH_NAMESPACE:-tekton-pipelines}"
# shellcheck source=test/performance/environments/openshift/common.sh
source "${HERE}/common.sh"

bench::resolve_cluster "$@"
bench::start_timer
bench::phase "phase 2: load (${CLUSTER})"

# Tier-2 defaults scale far above tier-1; override for a quick kind dry run.
COUNT="${BENCH_COUNT:-200000}"
CONCURRENCY="${BENCH_CONCURRENCY:-32}"
NAMESPACES="${BENCH_NAMESPACES:-200}"
if [ "${CLUSTER}" = "kind" ]; then
    COUNT="${BENCH_COUNT:-2000}"
    CONCURRENCY="${BENCH_CONCURRENCY:-8}"
    NAMESPACES="${BENCH_NAMESPACES:-50}"
fi

SSL_CERT_PATH="${SSL_CERT_PATH:-/tmp/tekton-results/ssl}"
SA_TOKEN_PATH="${SA_TOKEN_PATH:-/tmp/tekton-results/tokens}"
CERT="${SSL_CERT_PATH}/tekton-results-cert.pem"
TOKEN="${SA_TOKEN_PATH}/all-namespaces-admin-access"

if [ -n "${BENCH_SEED_DUMP:-}" ]; then
    echo "Restoring seed dataset from pg_dump snapshot ${BENCH_SEED_DUMP} (query-only seed; bypasses the API)..."
    pod="$(bench::kubectl get pods --namespace="${NAMESPACE}" -l app.kubernetes.io/name=postgresql -o name | head -n1)"
    if [ -z "${pod}" ]; then
        echo "error: could not find a Postgres pod to restore into" >&2
        exit 1
    fi
    bench::kubectl exec -i --namespace="${NAMESPACE}" "${pod}" -- \
        psql -U postgres -d tekton-results <"${BENCH_SEED_DUMP}"
    echo "Snapshot restored. Reminder: this seed is valid for query runs only."
    exit 0
fi

echo "Loading ${COUNT} PipelineRuns across ${NAMESPACES} namespaces at concurrency ${CONCURRENCY}..."
go run "${HARNESS}" load --verify \
    --cert "${CERT}" --token "${TOKEN}" \
    --count "${COUNT}" \
    --concurrency "${CONCURRENCY}" \
    --namespaces "${NAMESPACES}" \
    --dataset seed-large --dataset-version seed-large-v1

bench::phase "phase 2: load complete (${CLUSTER})"
