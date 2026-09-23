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
# Phase 4 — collect the tier-2 reports and print next steps.
#
# Validates each JSON report, prints a one-line summary, and reminds the operator
# how to compare against a baseline and where to commit the promoted results. The
# ephemeral cluster teardown is external (bookings expire); this phase only gathers
# artifacts before the cluster disappears.

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(git rev-parse --show-toplevel)"
HARNESS="${ROOT}/test/performance/harness"
# shellcheck source=test/performance/environments/openshift/common.sh
source "${HERE}/common.sh"

bench::resolve_cluster "$@"
bench::start_timer
bench::phase "phase 4: collect (${CLUSTER})"

OUTPUT_DIR="${BENCH_OUTPUT_DIR:-${ROOT}/test/performance/output/tier2}"
if [ ! -d "${OUTPUT_DIR}" ]; then
    echo "error: no reports found under ${OUTPUT_DIR}; run 03-benchmark.sh first" >&2
    exit 1
fi

found=0
for report in "${OUTPUT_DIR}"/*.json; do
    [ -e "${report}" ] || continue
    found=1
    if python3 -c "import json,sys; json.load(open(sys.argv[1]))" "${report}" >/dev/null 2>&1; then
        echo "ok: ${report}"
    else
        echo "FAIL: ${report} is not valid JSON" >&2
        exit 1
    fi
done

if [ "${found}" -eq 0 ]; then
    echo "error: ${OUTPUT_DIR} contains no JSON reports" >&2
    exit 1
fi

cat <<EOF

Phase 4 complete (cluster=${CLUSTER}). Reports collected in ${OUTPUT_DIR}.

Next steps:
  # Gate against a committed baseline (exits non-zero on regression):
  go run ${HARNESS} compare <baseline.json> ${OUTPUT_DIR}/query.json

  # Promote a validated run into the committed results tree:
  #   test/performance/results/<milestone>/tier2-<dataset-version>-<commit>.json
  # See test/performance/results/README.md for the milestone-gate process.
EOF
