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
# Create distinct Kubernetes identities and write one short-lived token per user.
set -euo pipefail
umask 077

COUNT=${1:-1000}
if [[ ! "$COUNT" =~ ^[1-9][0-9]{0,3}$ ]] || (( COUNT > 1000 )); then
    echo "Usage: $0 [user-count: 1-1000] [empty-token-directory]" >&2
    exit 1
fi
TOKEN_DIR=${2:-$(mktemp -d /tmp/results-benchmark-tokens.XXXXXX)}
mkdir -p "$TOKEN_DIR"
chmod 700 "$TOKEN_DIR"
if compgen -G "$TOKEN_DIR/*.token" > /dev/null; then
    echo "Use an empty token directory to avoid mixing old and new tokens." >&2
    exit 1
fi

# One binding grants all identities in this dedicated namespace Results access.
# This uses the Results admin role, not Kubernetes cluster-admin.
{
    cat <<'YAML'
apiVersion: v1
kind: Namespace
metadata:
  name: results-benchmark
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: results-benchmark
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: tekton-results-admin
subjects:
  - kind: Group
    apiGroup: rbac.authorization.k8s.io
    name: system:serviceaccounts:results-benchmark
YAML
    for (( i=1; i<=COUNT; i++ )); do
        printf -- '---\napiVersion: v1\nkind: ServiceAccount\nmetadata:\n  name: benchmark-%04d\n  namespace: results-benchmark\n' "$i"
    done
} | kubectl apply -f - >&2

for (( i=1; i<=COUNT; i++ )); do
    printf -v name 'benchmark-%04d' "$i"
    kubectl create token "$name" -n results-benchmark --duration="${TOKEN_DURATION:-24h}" > "$TOKEN_DIR/$name.token"
done
printf 'Created %s users. Token directory: %s\n' "$COUNT" "$TOKEN_DIR" >&2
# stdout contains only the path, for: TOKENS_DIR=$(.../04-create-users.sh 1000)
printf '%s\n' "$TOKEN_DIR"
