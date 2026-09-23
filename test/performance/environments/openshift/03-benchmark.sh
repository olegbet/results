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
# Phase 3 — run the store / query / mixed benchmarks at tier 2 and write reports.
#
# Reports land under BENCH_OUTPUT_DIR (default test/performance/output/tier2) tagged
# with --tier tier2. The query driver replays the committed, KubeArchive-derived
# query mix by default (see harness/workload); override with --query-spec.
#
# Tunables (env): BENCH_OUTPUT_DIR, BENCH_COUNT, BENCH_CONCURRENCY, BENCH_DURATION.

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(git rev-parse --show-toplevel)"
HARNESS="${ROOT}/test/performance/harness"
# shellcheck source=test/performance/environments/openshift/common.sh
source "${HERE}/common.sh"

bench::resolve_cluster "$@"
bench::start_timer
bench::phase "phase 3: benchmark (${CLUSTER})"

OUTPUT_DIR="${BENCH_OUTPUT_DIR:-${ROOT}/test/performance/output/tier2}"
mkdir -p "${OUTPUT_DIR}"

COUNT="${BENCH_COUNT:-200000}"
CONCURRENCY="${BENCH_CONCURRENCY:-32}"
DURATION="${BENCH_DURATION:-120}"
if [ "${CLUSTER}" = "kind" ]; then
    COUNT="${BENCH_COUNT:-2000}"
    CONCURRENCY="${BENCH_CONCURRENCY:-8}"
    DURATION="${BENCH_DURATION:-30}"
fi

SSL_CERT_PATH="${SSL_CERT_PATH:-/tmp/tekton-results/ssl}"
SA_TOKEN_PATH="${SA_TOKEN_PATH:-/tmp/tekton-results/tokens}"
CERT="${SSL_CERT_PATH}/tekton-results-cert.pem"
TOKEN="${SA_TOKEN_PATH}/all-namespaces-admin-access"

common=(--cert "${CERT}" --token "${TOKEN}" --tier tier2 --count "${COUNT}" --concurrency "${CONCURRENCY}")

bench::phase "phase 3: store"
go run "${HARNESS}" store "${common[@]}" --output "${OUTPUT_DIR}/store.json"

bench::phase "phase 3: query"
go run "${HARNESS}" query "${common[@]}" --transport both --duration "${DURATION}" --output "${OUTPUT_DIR}/query.json"

bench::phase "phase 3: mixed"
go run "${HARNESS}" mixed "${common[@]}" --duration "${DURATION}" --output "${OUTPUT_DIR}/mixed.json"

bench::phase "phase 3: benchmark complete (${CLUSTER})"
echo "Reports written to ${OUTPUT_DIR}"
