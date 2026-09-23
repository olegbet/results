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
# Shared helpers for the tier-2 benchmark phase scripts: --cluster resolution and
# 90-minute phase-budget timing. Source this file; do not execute it.
#
# The tier-2 pipeline targets an ephemeral multi-node OpenShift cluster, but every
# phase also accepts `--cluster kind` so the whole flow can be dry-run locally
# before booking OpenShift time. The chosen value is exported as BENCH_CLUSTER so
# it threads through the phases when they are run in the same shell; each phase
# also accepts its own --cluster flag.

# BENCH_BUDGET_SECONDS is the tier-2 session budget (default 90 minutes). Phase
# banners warn once elapsed time crosses it.
BENCH_BUDGET_SECONDS="${BENCH_BUDGET_SECONDS:-5400}"

# bench::resolve_cluster parses --cluster from the given args (falling back to the
# BENCH_CLUSTER env var, default "openshift"), validates it, and exports the result
# as both BENCH_CLUSTER and the CLUSTER shell variable.
bench::resolve_cluster() {
    local cluster="${BENCH_CLUSTER:-openshift}"
    while [ $# -gt 0 ]; do
        case "$1" in
        --cluster)
            cluster="${2:-}"
            shift 2 || shift
            ;;
        --cluster=*)
            cluster="${1#*=}"
            shift
            ;;
        *)
            shift
            ;;
        esac
    done
    case "${cluster}" in
    kind | openshift) ;;
    *)
        echo "error: --cluster must be 'kind' or 'openshift' (got '${cluster}')" >&2
        return 1
        ;;
    esac
    export BENCH_CLUSTER="${cluster}"
    # CLUSTER is consumed by the phase scripts that source this file.
    # shellcheck disable=SC2034
    CLUSTER="${cluster}"
}

# bench::start_timer records the phase-timer origin for this script.
bench::start_timer() {
    BENCH_T0="$(date +%s)"
}

# bench::phase prints a banner with elapsed time and warns when the budget is
# exceeded, so an operator can keep the whole session inside the 90-minute window.
bench::phase() {
    local name="$1"
    : "${BENCH_T0:=$(date +%s)}"
    local now elapsed
    now="$(date +%s)"
    elapsed=$((now - BENCH_T0))
    printf '== %s == (elapsed %dm%02ds / budget %dm)\n' \
        "${name}" $((elapsed / 60)) $((elapsed % 60)) $((BENCH_BUDGET_SECONDS / 60))
    if [ "${elapsed}" -gt "${BENCH_BUDGET_SECONDS}" ]; then
        echo "warning: tier-2 session budget of $((BENCH_BUDGET_SECONDS / 60)) minutes exceeded (${elapsed}s elapsed)" >&2
    fi
}

# bench::kubectl runs kubectl (OpenShift's oc is compatible for the verbs used
# here); callers rely on the active KUBECONFIG context.
bench::kubectl() {
    kubectl "$@"
}
