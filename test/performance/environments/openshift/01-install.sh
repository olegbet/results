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
# Phase 1 — install Tekton Results + Postgres for a tier-2 run.
#
# openshift: install the app, then apply the tier-2 Postgres tuning (dedicated-node
#            nodeSelector + larger postgresql.conf). Set BENCH_DB_URL to use an
#            external managed Postgres instead of the in-cluster one.
# kind:      delegate to the tier-1 local-DB installer; the tier-2 Postgres tuning
#            (dedicated node) does not apply on a single-node kind cluster.

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(git rev-parse --show-toplevel)"
NAMESPACE="${BENCH_NAMESPACE:-tekton-pipelines}"
# shellcheck source=test/performance/environments/openshift/common.sh
source "${HERE}/common.sh"

bench::resolve_cluster "$@"
bench::start_timer
bench::phase "phase 1: install (${CLUSTER})"

if [ "${CLUSTER}" = "kind" ]; then
    echo "kind mode: installing the tier-1 local-DB deployment."
    "${ROOT}/test/performance/environments/kind/01-install-localdb.sh"
    exit 0
fi

export SSL_INCLUDE_LOCALHOST="${SSL_INCLUDE_LOCALHOST:-true}"

# On OpenShift the Tekton and Results pods are rejected by the default
# restricted-v2 SCC for two independent reasons: they request UID 65532 (outside
# the namespace's allocated UID range) and they set the deprecated
# container.seccomp.security.alpha.kubernetes.io annotation. The privileged SCC
# admits both; anyuid does not (it rejects the seccomp annotation). Grant it to
# every service account in the namespace up front, so pods are admitted the first
# time they are scheduled -- the e2e installer below waits on pod readiness
# internally and would time out if the grant landed afterwards.
if command -v oc &> /dev/null; then
    echo "OpenShift detected: granting privileged SCC to service accounts in ${NAMESPACE}..."
    oc create namespace "${NAMESPACE}" 2>/dev/null || true
    oc adm policy add-scc-to-group privileged "system:serviceaccounts:${NAMESPACE}"
fi

echo "Installing Tekton Results (standard e2e deployment)..."
"${ROOT}/test/e2e/01-install.sh"

if [ -n "${BENCH_DB_URL:-}" ]; then
    echo "External database configured via BENCH_DB_URL; skipping in-cluster Postgres tuning."
    echo "Ensure the API server Secret points at the external instance."
else
    echo "Applying tier-2 Postgres tuning (dedicated node + tuned postgresql.conf)..."
    bench::kubectl create configmap postgres-tuning \
        --namespace="${NAMESPACE}" \
        --from-file=postgresql.conf="${HERE}/postgresql.conf" \
        --dry-run=client -o yaml | bench::kubectl apply -f -

    bench::kubectl patch statefulset tekton-results-postgres \
        --namespace="${NAMESPACE}" \
        --type=strategic \
        --patch-file "${HERE}/postgres-patch.yaml"

    echo "Waiting for Postgres to roll out with tuning applied..."
    bench::kubectl rollout status statefulset/tekton-results-postgres --namespace="${NAMESPACE}" --timeout=300s
fi

bench::kubectl wait deployment tekton-results-api --namespace="${NAMESPACE}" --for=condition=available --timeout=180s

cat <<EOF

Phase 1 complete (cluster=${CLUSTER}).

Set the connection env before loading:
  export API_SERVER_ADDR=<api route or forwarded address>
  export SSL_CERT_PATH=/tmp/tekton-results/ssl
  export SA_TOKEN_PATH=/tmp/tekton-results/tokens
EOF
