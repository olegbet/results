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
# Phase 0 — select and validate the target cluster for a tier-2 benchmark.
#
#   ./00-cluster.sh --cluster openshift   # (default) attach to a booked cluster
#   ./00-cluster.sh --cluster kind        # local dry run of the tier-2 pipeline
#
# openshift: the ephemeral multi-node cluster is provisioned externally; this
#            script only validates that $KUBECONFIG points at a reachable,
#            preferably multi-node cluster.
# kind:      delegates to the tier-1 kind bootstrap so the full pipeline can be
#            rehearsed locally. Results are dev-only, NOT a tier-2 baseline.

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=test/performance/environments/openshift/common.sh
source "${HERE}/common.sh"

bench::resolve_cluster "$@"
bench::start_timer
bench::phase "phase 0: cluster (${CLUSTER})"

if [ "${CLUSTER}" = "kind" ]; then
    echo "kind mode: dry run only — results are NOT a tier-2 baseline."
    if kubectl cluster-info >/dev/null 2>&1; then
        echo "Reusing the current kube context."
    else
        echo "No reachable cluster; standing up the tier-1 kind cluster..."
        "${ROOT}/test/performance/environments/kind/00-kind-up.sh"
    fi
else
    echo "openshift mode: validating the booked ephemeral cluster in \$KUBECONFIG."
    if ! kubectl cluster-info >/dev/null 2>&1; then
        echo "error: no reachable cluster; point \$KUBECONFIG at the booked OpenShift cluster" >&2
        exit 1
    fi
    nodes="$(kubectl get nodes --no-headers 2>/dev/null | wc -l | tr -d ' ')"
    echo "Cluster reachable with ${nodes} node(s)."
    if [ "${nodes}" -lt 3 ]; then
        echo "warning: expected a multi-node cluster for a representative tier-2 run (got ${nodes})" >&2
    fi
fi

cat <<EOF

Phase 0 complete (cluster=${CLUSTER}).

Thread the same choice through the remaining phases, e.g.:

  export BENCH_CLUSTER=${CLUSTER}
  ./01-install.sh
  ./02-load.sh
  ./03-benchmark.sh
  ./04-collect.sh

or pass --cluster ${CLUSTER} to each phase explicitly.
EOF
