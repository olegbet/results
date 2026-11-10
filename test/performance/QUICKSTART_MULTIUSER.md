# Quickstart Validation

This guide describes live validation of the implemented benchmark interfaces. Local fixture
tests do not establish cache acceptance. The procedures below have not been live-executed.
Run shell examples from the repository root.

## Prerequisites

Use the repository's Go toolchain (1.26.10), existing Kind/ko/kubectl installation prerequisites,
jq, and a disposable test cluster. Docker node access is needed for the optional audit procedure.
Supply an accessible image built from the reviewed cache PR; the current installer builds this
worktree and must not be assumed to install that PR.

For a new disposable Kind environment:

```bash
set -euo pipefail
umask 077
BENCH_WORK=$(mktemp -d /tmp/results-cache-validation.XXXXXX)
export KUBECONFIG="$BENCH_WORK/kubeconfig"
BENCH_CONTEXT=kind-tekton-results
go build -o "$BENCH_WORK/bench" ./test/performance/benchmark
```

## Build the cache PR image locally

No prebuilt PR image is required. Build in a separate checkout so the benchmark
worktree and its uncommitted changes remain intact. These commands need network
access, Git, Docker and ko; they create a local image without pushing a registry.
The Go version comes from the pinned PR checkout. Keep the exact revision, build
receipt and local image identity with the measurement artifacts.

```bash
BENCH_CACHE_REV=2b443c443b314d95f5acc14cfdba7532babf049a
BENCH_CACHE_SOURCE="$BENCH_WORK/cache-pr-source"
git clone --no-checkout https://github.com/tektoncd/results.git "$BENCH_CACHE_SOURCE"
git -C "$BENCH_CACHE_SOURCE" fetch origin refs/pull/1529/head
git -C "$BENCH_CACHE_SOURCE" checkout --detach "$BENCH_CACHE_REV"
test "$(git -C "$BENCH_CACHE_SOURCE" rev-parse HEAD)" = "$BENCH_CACHE_REV"
# Match the Kind nodes' architecture; override for arm64 hosts.
BENCH_IMAGE_PLATFORM=${BENCH_IMAGE_PLATFORM:-linux/amd64}
(
  cd "$BENCH_CACHE_SOURCE" || exit
  KO_DOCKER_REPO=ko.local ko build --local --tag-only \
    --platform "$BENCH_IMAGE_PLATFORM" --tags=pr1529-2b443c443b31 \
    --image-label "org.opencontainers.image.revision=$BENCH_CACHE_REV" \
    ./cmd/api
) > "$BENCH_WORK/cache-image-build.txt"
CACHE_API_IMAGE=$(cat "$BENCH_WORK/cache-image-build.txt")
docker image inspect "$CACHE_API_IMAGE" \
  --format '{{json .}}' | jq '{Id,RepoTags,Architecture,Os,
    revision:.Config.Labels["org.opencontainers.image.revision"]}' \
  > "$BENCH_WORK/cache-image-receipt.json"
jq -e --arg revision "$BENCH_CACHE_REV" '.revision==$revision' \
  "$BENCH_WORK/cache-image-receipt.json"
```

If fetching the PR head no longer makes the pinned commit available, explicitly
fetch that exact SHA from the upstream repository; stop if it cannot be resolved.
Do not silently build the latest PR head or this benchmark worktree's API.
The image uses the PR checkout's `.ko.yaml` base and vendored dependencies.

## Install and select the local image

Create the cluster using [Metadata-only audit setup](contracts/cache-evidence.md#create-a-cluster-with-metadata-only-audit)
for cache conclusions. If using the ordinary setup instead, reports remain useful but cache
conclusions stay unverified without another adequate evidence source. Continue after cluster creation:

```bash
./test/performance/environments/kind/01-install-localdb.sh
# The installer builds this worktree; explicitly replace its API image below.
kind load docker-image "$CACHE_API_IMAGE" --name tekton-results
kubectl --context "$BENCH_CONTEXT" -n tekton-pipelines patch deployment tekton-results-api \
  --type=strategic -p '{"spec":{"template":{"spec":{"containers":[{"name":"api","imagePullPolicy":"IfNotPresent"}]}}}}'
kubectl --context "$BENCH_CONTEXT" -n tekton-pipelines set image \
  deployment/tekton-results-api "api=$CACHE_API_IMAGE"
kubectl --context "$BENCH_CONTEXT" -n tekton-pipelines scale \
  deployment/tekton-results-api --replicas=1
kubectl --context "$BENCH_CONTEXT" -n tekton-pipelines set env \
  deployment/tekton-results-api AUTH_DISABLE=false AUTH_CACHE_ENABLED=false \
  AUTH_CACHE_POSITIVE_TTL=15m AUTH_CACHE_NEGATIVE_TTL=15m
kubectl --context "$BENCH_CONTEXT" -n tekton-pipelines rollout status \
  deployment/tekton-results-api --timeout=180s

kubectl --context "$BENCH_CONTEXT" create namespace benchmark-users
kubectl --context "$BENCH_CONTEXT" create namespace ns-00
kubectl --context "$BENCH_CONTEXT" create namespace ns-01

BENCH_CERT=/tmp/tekton-results/ssl/tekton-results-cert.pem
BENCH_ADMIN=/tmp/tekton-results/tokens/all-namespaces-admin-access
BENCH_COMMON=(--server-addr https://localhost:8080 --cert "$BENCH_CERT"
  --server-image "$CACHE_API_IMAGE" --dataset cache-validation
  --dataset-version cache-validation-v1 --seed 42 --namespaces 1)
```

Verify the deployed container image and runtime image ID after rollout, and bind the
image receipt to the environment controls. `kind load` must be repeated for a recreated
cluster; this local image is not available to remote clusters. For a remote cluster,
push the same pinned build to an accessible registry and use its immutable digest.

The API NodePort routes to the single ready API replica. Verify no second API pod is serving and
record its identity; do not roll the process between cold/warm/expiry passes. For a different
deployment, use its explicit endpoint, certificate, and controlled pod routing.

## Seed data and prepare 1,000 users

```bash
"$BENCH_WORK/bench" dataset --count 1000 --seed 42 --namespaces 1 \
  --dataset cache-validation --dataset-version cache-validation-v1 \
  --output "$BENCH_WORK/dataset.json"

"$BENCH_WORK/bench" load "${BENCH_COMMON[@]}" --token "$BENCH_ADMIN" \
  --count 1000 --verify --output "$BENCH_WORK/load.json"

"$BENCH_WORK/bench" users prepare --context "$BENCH_CONTEXT" \
  --namespace benchmark-users --access-namespaces ns-00 --profile read-write \
  --count 1000 --token-duration 2h --output-dir "$BENCH_WORK/users"

jq -e '.clients | length == 1000' "$BENCH_WORK/users/pool.json"
cat > "$BENCH_WORK/cases.json" <<'JSON'
{"schema_version":"1","cases":[
  {"id":"results-list","operation":"list-results","parent":"ns-00","expected":"allow"}
]}
JSON
```

Expected: 1,000 distinct service-account identities and token files, ready ownership state,
private file permissions, and a successful seed verification. Inspect actual earliest token
expiry rather than assuming the requested two hours was granted. Seed preparation is outside
measurement; establish a new cache process after it.

## Record environment controls

Before measurement, create a sanitized receipt for the effective environment. This initial
projection records API resource settings without exporting environment values or Secrets:

```bash
kubectl --context "$BENCH_CONTEXT" -n tekton-pipelines \
  get deployment tekton-results-api -o json |
  jq '{schema_version:"1",
    client:{effective_cpu:null,effective_memory_bytes:null},
    api:{replicas:.spec.replicas,
      containers:[.spec.template.spec.containers[] |
        {name,resources:(.resources // {})}],node_capacity:null},
    database:{version:null,topology:"local",resources:null,storage:null,
      connection_limits:null,results_connection_pool:null},
    verification:{complete:false,
      sources:["API deployment resource declarations"],
      unknown_reasons:["Client, serving-node, and effective database controls need verification"]}
  }' > "$BENCH_WORK/environment.json"
```

Complete the null values from the actual environment before accepting a controlled comparison:

- Record client CPU/memory available to the benchmark process, accounting for container/VM
  restrictions rather than assuming the host totals are available.
- Check the serving API pod's applied requests/limits against the deployment and record node
  capacity. Record an absent limit explicitly as unlimited, not unknown.
- Record the database version, topology, CPU/memory/storage capacity and limits, database
  connection limits, and effective Results database pool settings. Use allowlisted non-secret
  configuration and operator/provider evidence; do not export Secret contents or full connection
  strings. No database reads are introduced into measured workloads.
- Record the source and verification status of each completed group. Keep unavailable values null
  with a reason and `verification.complete=false`; such a receipt cannot certify a controlled
  comparison. Mark complete only after every required control is verified.

Retain this receipt unchanged once referenced by a report. Recheck the controls before each
phase; use a new receipt and reference if they change. Compare controls across phases independently
of the intended cache-setting change, and bind the receipt's exact SHA-256 in each external
assessment. The benchmark validates the reference string but does not inspect the receipt.

## Record disabled-cache context

```bash
BENCH_POD=$(kubectl --context "$BENCH_CONTEXT" -n tekton-pipelines get pods \
  -l app.kubernetes.io/name=tekton-results-api -o json |
  jq -er '[.items[] | select(.metadata.deletionTimestamp == null)] |
    if length == 1 then .[0].metadata.name else error("expected one API pod") end')

kubectl --context "$BENCH_CONTEXT" -n tekton-pipelines get pod "$BENCH_POD" -o json |
  jq '{name:.metadata.name, uid:.metadata.uid, created:.metadata.creationTimestamp,
       service_account:.spec.serviceAccountName,
       images:[.spec.containers[].image],
       api_process:[.status.containerStatuses[] | select(.name=="api") |
         {container_id:.containerID,restart_count:.restartCount,
          started_at:.state.running.startedAt}]}' > "$BENCH_WORK/disabled-pod.json"

jq -n --arg image "$CACHE_API_IMAGE" --arg pod "$BENCH_POD" \
  --arg configuration "$BENCH_WORK/environment.json" '{
  schema_version:"1", scenario_id:"scale-1000", phase:"disabled",
  cache:{enabled:false,positive_ttl:"15m",negative_ttl:"15m",capacity_per_cache:10000,
    provenance:"operator-declared"},
  environment:{server_image:$image,
    server_revision:"2b443c443b314d95f5acc14cfdba7532babf049a",
    replicas:1,selected_pod:$pod,routing:"single-replica-nodeport",
    authentication_enabled:true,k8s_qps:500,k8s_burst:1000,db_type:"local",
    configuration_ref:$configuration,
    provenance:"operator-declared"},
  control:{working_set:"1000 users; ns-00; results/list",exclusive_traffic:true,
    cold_state_ref:"",prior_report_ref:"",provenance:"operator-declared"}
}' > "$BENCH_WORK/disabled-context.json"
```

Check effective image/revision, auth mode, TTLs, QPS/burst, and replica/routing controls against
the deployment. The example labels these values operator-declared until independently checked;
do not treat an environment-variable assignment as verification. Update recorded values if the
selected image/configuration differs.

## Configure automatic metric snapshots

The existing Results metrics listener serves plain HTTP on port 9090, separately from the
workload API. In a separate terminal with the same kubeconfig/context and selected pod values,
start a loopback-only forward and leave it running:

```bash
kubectl --context "$BENCH_CONTEXT" -n tekton-pipelines \
  port-forward "pod/$BENCH_POD" 19090:9090 --address=127.0.0.1
```

Restart this forward whenever an API rollout selects a new pod. Verify readiness before the
measurement bookmark; the direct metrics listener does not authenticate through TokenReview/SAR.
Do not silently replace it with an authenticated proxy that warms the cache or adds review calls.

In the main shell, create a configuration using existing process/request metrics. The exact gRPC
series may be absent before its first request; that is unavailable, not an inferred zero baseline.

```bash
curl --fail --silent --show-error --output /dev/null http://127.0.0.1:19090/metrics
BENCH_POD_UID=$(kubectl --context "$BENCH_CONTEXT" -n tekton-pipelines \
  get pod "$BENCH_POD" -o jsonpath='{.metadata.uid}')
jq -n --arg instance "$BENCH_POD_UID" '{
  schema_version:"1", sources:[{
    id:"results-api",url:"http://127.0.0.1:19090/metrics",instance_id:$instance,
    exclusive_traffic:true,scope_evidence_ref:"traffic-control.json",timeout:"5s",
    selectors:[
      {id:"list-results-completed",metric:"grpc_server_handled_total",type:"counter",
       labels:{grpc_type:"unary",grpc_service:"tekton.results.v1alpha2.Results",
         grpc_method:"ListResults",grpc_code:"OK"},signal:"supporting"},
      {id:"api-cpu",metric:"process_cpu_seconds_total",type:"counter",labels:{},signal:"supporting"},
      {id:"api-memory",metric:"process_resident_memory_bytes",type:"gauge",labels:{},signal:"supporting"}
    ]
  }]
}' > "$BENCH_WORK/metrics.json"
BENCH_CAPTURE=(--metrics-config "$BENCH_WORK/metrics.json")
```

Create `traffic-control.json` beside this configuration to record how the instance is pinned,
how other API traffic is excluded, and that the collector uses the separate listener. This is
operator evidence to check, not an assertion the benchmark independently verifies. If isolation
cannot be established, use `exclusive_traffic:false`; samples remain available but counter
attribution remains unavailable. Never place credentials in labels or evidence references.

The [metrics contract](contracts/metrics.md) defines exact full-label selectors, bounded capture,
process continuity, and safe reporting. Direct cache hit/miss counters are absent from the targeted
PR and remain unavailable/not-configured; these process/gRPC measurements do not replace them.
A separately exposed, sufficiently scoped review counter may be explicitly configured after
verifying its metric/label mapping. Metadata audit remains the reference evidence in this guide.

## Verify coverage at low and high concurrency

For evidence, bracket each finite command using the collection procedure in
[cache-evidence.md](contracts/cache-evidence.md#bracket-one-finite-workload).

```bash
for BENCH_CONCURRENCY in 8 1000; do
  "$BENCH_WORK/bench" query "${BENCH_COMMON[@]}" "${BENCH_CAPTURE[@]}" \
    --token-pool "$BENCH_WORK/users/pool.json" --access-cases "$BENCH_WORK/cases.json" \
    --run-context "$BENCH_WORK/disabled-context.json" \
    --dataset-count 1000 --count 1000 --concurrency "$BENCH_CONCURRENCY" \
    --transport grpc --output "$BENCH_WORK/disabled-$BENCH_CONCURRENCY.json" \
    > "$BENCH_WORK/disabled-$BENCH_CONCURRENCY.stdout.json"
  cmp "$BENCH_WORK/disabled-$BENCH_CONCURRENCY.json" \
    "$BENCH_WORK/disabled-$BENCH_CONCURRENCY.stdout.json"
  jq -e '.config.client_pool.configured_clients == 1000
    and .config.client_pool.supported_distinct_users == 1000
    and .validation.coverage.exercised_clients == 1000
    and .validation.coverage.successful_clients == 1000
    and .validation.responses.status == "passed"
    and .metrics.total_errors == 0
    and ([.metrics.clients[].total_ops] | add) == .metrics.total_ops' \
    "$BENCH_WORK/disabled-$BENCH_CONCURRENCY.json"
done
```

The request count is global, not 1,000 per user. Large list responses may make concurrency 1,000
resource-intensive; provision adequate client/server capacity and retain actual failures.
Inspect `metrics.capture.sources` for before/after values and `validation.metrics_capture` for
collection availability. Missing direct cache signals must be unavailable, never zero. Capture
windows bracket `validation.measurement_window`; they do not inflate workload duration. Check
process-start continuity and preserve partial samples if a source or selector is unavailable.

## Cold and warm passes with a stable population

Enable caching, wait for the new process, and record a new pod receipt:

```bash
kubectl --context "$BENCH_CONTEXT" -n tekton-pipelines set env \
  deployment/tekton-results-api AUTH_CACHE_ENABLED=true
kubectl --context "$BENCH_CONTEXT" -n tekton-pipelines rollout status \
  deployment/tekton-results-api --timeout=180s
BENCH_POD=$(kubectl --context "$BENCH_CONTEXT" -n tekton-pipelines get pods \
  -l app.kubernetes.io/name=tekton-results-api -o json |
  jq -er '[.items[] | select(.metadata.deletionTimestamp == null)] |
    if length == 1 then .[0].metadata.name else error("expected one API pod") end')
kubectl --context "$BENCH_CONTEXT" -n tekton-pipelines get pod "$BENCH_POD" -o json |
  jq '{name:.metadata.name,uid:.metadata.uid,created:.metadata.creationTimestamp,
       images:[.spec.containers[].image],
       api_process:[.status.containerStatuses[] | select(.name=="api") |
         {container_id:.containerID,restart_count:.restartCount,
          started_at:.state.running.startedAt}]}' > "$BENCH_WORK/cold-pod.json"

# Restart the separate-terminal metrics forward for BENCH_POD before continuing.
BENCH_POD_UID=$(kubectl --context "$BENCH_CONTEXT" -n tekton-pipelines \
  get pod "$BENCH_POD" -o jsonpath='{.metadata.uid}')
jq --arg instance "$BENCH_POD_UID" '.sources[0].instance_id=$instance' \
  "$BENCH_WORK/metrics.json" > "$BENCH_WORK/metrics-next.json"
mv "$BENCH_WORK/metrics-next.json" "$BENCH_WORK/metrics.json"
curl --fail --silent --show-error --output /dev/null http://127.0.0.1:19090/metrics

jq --arg pod "$BENCH_POD" --arg receipt "$BENCH_WORK/cold-pod.json" \
  '.phase="cold" | .cache.enabled=true | .environment.selected_pod=$pod |
   .control.cold_state_ref=$receipt' "$BENCH_WORK/disabled-context.json" \
  > "$BENCH_WORK/cold-context.json"

"$BENCH_WORK/bench" query "${BENCH_COMMON[@]}" "${BENCH_CAPTURE[@]}" \
  --token-pool "$BENCH_WORK/users/pool.json" --access-cases "$BENCH_WORK/cases.json" \
  --run-context "$BENCH_WORK/cold-context.json" --dataset-count 1000 --count 1000 \
  --concurrency 8 --transport grpc --output "$BENCH_WORK/cold.json"

jq --arg prior "$BENCH_WORK/cold.json" \
  '.phase="warm" | .control.prior_report_ref=$prior | .control.cold_report_ref=$prior' "$BENCH_WORK/cold-context.json" \
  > "$BENCH_WORK/warm-context.json"

"$BENCH_WORK/bench" query "${BENCH_COMMON[@]}" "${BENCH_CAPTURE[@]}" \
  --token-pool "$BENCH_WORK/users/pool.json" --access-cases "$BENCH_WORK/cases.json" \
  --run-context "$BENCH_WORK/warm-context.json" --dataset-count 1000 --count 1000 \
  --concurrency 8 --transport grpc --output "$BENCH_WORK/warm.json"
```

Bracket the cold and warm commands separately for audit evidence. Do not restart the API between
them. Verify the warm pass finishes within the configured lifetime for every cold entry.
Expected: full response coverage passes; the emitted cache assessment is unverified until the
external assessment is completed. Under complete controls, cold has review activity and warm
has no additional relevant reviews.

## Separate positive and negative decision expiry

Use a small functional pool with distinct allowed and denied identities; these stages cannot be
isolated using only allowed users. Prepare a none-profile account and combine it with one allowed
identity, using absolute private paths so moving the combined manifest does not change resolution:

```bash
"$BENCH_WORK/bench" users prepare --context "$BENCH_CONTEXT" \
  --namespace benchmark-users --profile none --count 1 --token-duration 2h \
  --output-dir "$BENCH_WORK/ttl-denied"
jq -n --slurpfile allowed "$BENCH_WORK/users/pool.json" \
  --slurpfile denied "$BENCH_WORK/ttl-denied/pool.json" --arg dir "$BENCH_WORK" '{
  schema_version:"1",population_id:"ttl-decision-users",clients:[
    ($allowed[0].clients[0] | .id="allowed-user" |
      .token_file=($dir+"/users/"+.token_file) |
      .identity.evidence_ref=($dir+"/users/"+.identity.evidence_ref)),
    ($denied[0].clients[0] | .id="denied-user" |
      .token_file=($dir+"/ttl-denied/"+.token_file) |
      .identity.evidence_ref=($dir+"/ttl-denied/"+.identity.evidence_ref))
  ]
}' > "$BENCH_WORK/ttl-pool.json"
cat > "$BENCH_WORK/ttl-cases.json" <<'JSON'
{"schema_version":"1","cases":[
  {"id":"decision-list","operation":"list-results","parent":"ns-00",
   "expected":"allow","expected_by_client":{"denied-user":"deny"}}
]}
JSON
```

Before running these experiments, optionally prepare the definitive invalid-credential cohort
using the [decision-only check](#permission-isolation-and-intentionally-invalid-credentials) below
and add it to this pool/case file. Use a separate token per cohort so refreshing authentication
for one does not change another. Without that fixture, retain invalid-cache conclusions as unverified.

First verify the completed matrix with caching disabled. A none profile may inherit permissions;
unexpected allow means the denied fixture is not valid and must be corrected before inference.
This fixture check precedes establishment of each experiment's fresh cache process:

```bash
kubectl --context "$BENCH_CONTEXT" -n tekton-pipelines set env \
  deployment/tekton-results-api AUTH_CACHE_ENABLED=false
kubectl --context "$BENCH_CONTEXT" -n tekton-pipelines rollout status \
  deployment/tekton-results-api --timeout=180s
BENCH_TTL_COUNT=$(jq '.clients|length' "$BENCH_WORK/ttl-pool.json")
"$BENCH_WORK/bench" query "${BENCH_COMMON[@]}" \
  --token-pool "$BENCH_WORK/ttl-pool.json" --access-cases "$BENCH_WORK/ttl-cases.json" \
  --dataset-count 1000 --count "$BENCH_TTL_COUNT" --concurrency 1 --transport grpc \
  --output "$BENCH_WORK/ttl-fixture-check.json"
jq -e '.validation.responses.status == "passed"' "$BENCH_WORK/ttl-fixture-check.json"
```

Establish the expected review stages with disabled-cache audit evidence as described in the
evidence guide; public rejections alone cannot distinguish denied authorization from failed
authentication. Select/record the new disabled pod for those bookmarks before the check above.

Run each cohort separately so aggregate audit counts cannot hide an incorrect decision in another
cohort. For each selected client, run both `positive` and `negative` short-lifetime experiments.
Repeat the following blocks with `BENCH_TTL_CLIENT=allowed-user`, then `denied-user`, and optionally
`invalid-user` after definitive fixture verification. Each experiment begins with a new API process:

```bash
BENCH_TTL_CLIENT=allowed-user
jq --arg client "$BENCH_TTL_CLIENT" '.clients |= map(select(.id==$client)) |
  if (.clients|length)==1 then . else error("expected one cohort client") end' \
  "$BENCH_WORK/ttl-pool.json" > "$BENCH_WORK/ttl-selected-pool.json"
jq --arg client "$BENCH_TTL_CLIENT" '.cases |= map(
  .expected=((.expected_by_client // {})[$client] // .expected) |
  del(.expected_by_client))' "$BENCH_WORK/ttl-cases.json" > "$BENCH_WORK/ttl-selected-cases.json"
```

Set the short lifetime for this cohort:

```bash
BENCH_SHORT_DECISION=positive
BENCH_TTL_EXPERIMENT="$BENCH_TTL_CLIENT-$BENCH_SHORT_DECISION"
BENCH_POSITIVE_TTL=60s
BENCH_NEGATIVE_TTL=10m
if [ "$BENCH_SHORT_DECISION" = negative ]; then
  BENCH_POSITIVE_TTL=10m
  BENCH_NEGATIVE_TTL=60s
fi
kubectl --context "$BENCH_CONTEXT" -n tekton-pipelines set env \
  deployment/tekton-results-api AUTH_CACHE_ENABLED=true \
  "AUTH_CACHE_POSITIVE_TTL=$BENCH_POSITIVE_TTL" "AUTH_CACHE_NEGATIVE_TTL=$BENCH_NEGATIVE_TTL"
# Explicitly restart even if the selected settings match an earlier experiment.
kubectl --context "$BENCH_CONTEXT" -n tekton-pipelines rollout restart deployment/tekton-results-api
kubectl --context "$BENCH_CONTEXT" -n tekton-pipelines rollout status \
  deployment/tekton-results-api --timeout=180s
BENCH_POD=$(kubectl --context "$BENCH_CONTEXT" -n tekton-pipelines get pods \
  -l app.kubernetes.io/name=tekton-results-api -o json |
  jq -er '[.items[] | select(.metadata.deletionTimestamp == null)] |
    if length == 1 then .[0].metadata.name else error("expected one API pod") end')
kubectl --context "$BENCH_CONTEXT" -n tekton-pipelines get pod "$BENCH_POD" -o json |
  jq '{name:.metadata.name,uid:.metadata.uid,
    api_process:[.status.containerStatuses[] | select(.name=="api") |
      {container_id:.containerID,restart_count:.restartCount,started_at:.state.running.startedAt}]}' \
  > "$BENCH_WORK/$BENCH_TTL_EXPERIMENT-pod.json"
```

Restart the metrics port-forward for the selected pod and update its declared identity. Recheck
the immutable environment receipt's controls. Create a context for this experiment and then run
three phases; bracket each query with audit collection from the evidence guide when assessing
cache behavior. The query commands alone deliberately leave the cache assessment unverified.

```bash
BENCH_POD_UID=$(kubectl --context "$BENCH_CONTEXT" -n tekton-pipelines \
  get pod "$BENCH_POD" -o jsonpath='{.metadata.uid}')
jq --arg instance "$BENCH_POD_UID" '.sources[0].instance_id=$instance' \
  "$BENCH_WORK/metrics.json" > "$BENCH_WORK/metrics-next.json"
mv "$BENCH_WORK/metrics-next.json" "$BENCH_WORK/metrics.json"
curl --fail --silent --show-error --output /dev/null http://127.0.0.1:19090/metrics
BENCH_TTL_COUNT=$(jq '.clients|length' "$BENCH_WORK/ttl-selected-pool.json")
jq --arg scenario "$BENCH_TTL_EXPERIMENT-expiry" --arg pod "$BENCH_POD" \
  --arg pos "$BENCH_POSITIVE_TTL" --arg neg "$BENCH_NEGATIVE_TTL" \
  --arg receipt "$BENCH_WORK/$BENCH_TTL_EXPERIMENT-pod.json" \
  --arg scope "$BENCH_TTL_CLIENT isolated cohort; ns-00; results/list" \
  '.scenario_id=$scenario | .phase="cold" | .cache.enabled=true |
   .cache.positive_ttl=$pos | .cache.negative_ttl=$neg | .environment.selected_pod=$pod |
   .control.cold_state_ref=$receipt | .control.working_set=$scope' \
  "$BENCH_WORK/disabled-context.json" > "$BENCH_WORK/ttl-context.json"

for BENCH_PHASE in cold warm "after-$BENCH_SHORT_DECISION-expiry"; do
  if [ "$BENCH_PHASE" != cold ]; then
    jq --arg phase "$BENCH_PHASE" --arg prior "$BENCH_TTL_PRIOR" \
      --arg cold "$BENCH_WORK/$BENCH_TTL_EXPERIMENT-cold.json" \
      '.phase=$phase | .control.prior_report_ref=$prior | .control.cold_report_ref=$cold' \
      "$BENCH_WORK/ttl-context.json" > "$BENCH_WORK/ttl-next.json"
    mv "$BENCH_WORK/ttl-next.json" "$BENCH_WORK/ttl-context.json"
  fi
  if [ "$BENCH_PHASE" != cold ] && [ "$BENCH_PHASE" != warm ]; then
    sleep 62
  fi
  # Apply before/after audit bookmarks around this finite query for independent evidence.
  "$BENCH_WORK/bench" query "${BENCH_COMMON[@]}" "${BENCH_CAPTURE[@]}" \
    --token-pool "$BENCH_WORK/ttl-selected-pool.json" --access-cases "$BENCH_WORK/ttl-selected-cases.json" \
    --run-context "$BENCH_WORK/ttl-context.json" --dataset-count 1000 \
    --count "$BENCH_TTL_COUNT" --concurrency 1 --transport grpc \
    --output "$BENCH_WORK/$BENCH_TTL_EXPERIMENT-$BENCH_PHASE.json"
  BENCH_TTL_PRIOR="$BENCH_WORK/$BENCH_TTL_EXPERIMENT-$BENCH_PHASE.json"
done
```

Run the second experiment by changing the first assignment to `BENCH_SHORT_DECISION=negative`
and repeating the deployment/context/phase blocks, including the rollout and metrics forward.
Then select the next cohort and repeat both experiments. Report filenames include cohort and
short-lifetime class; preserve earlier reports when rerunning an experiment. Warm must finish before
cold start + 60s - margin; expiry must start after cold end + 60s + margin and finish before cold
start + 10m - margin. The conservative sleep starts after warm; still verify these inequalities
from actual windows, including capture overhead. If they fail, widen separation and repeat.

Expected with short positive TTL: allowed cases renew TR+SAR, denied cases renew TR while the
negative SAR decision remains cached. With short negative TTL: allowed cases reuse both decisions,
denied cases renew SAR while successful authentication stays cached. If a definitive invalid
cohort is included, it reuses negative authentication in the first experiment and renews TR only
in the second; it never contributes SAR. See the full [decision matrix](contracts/cache-evidence.md#assess-the-four-conditions).

## Permission isolation and intentionally invalid credentials

Run these diagnostics first with caching disabled, then repeat controlled cold/warm/expiry passes.
A denied-cache assessment requires independent fixture evidence that the token authenticates
successfully and the exact identity/access tuple returns SAR `Allowed=false` with empty
`EvaluationError`. A public denial and HTTP-successful Metadata audit event alone cannot prove
this: review errors can cause the same public rejection. Use an operator with review permissions
to check outside measurement, retaining only authenticated/allowed/error-present flags and the
non-secret identity/access tuple (never raw tokens, review request bodies, or error text). Account
for TokenReview-supplied groups/extra attributes in the SAR. If this evidence is unavailable,
response checks remain useful but the denied-cache branch stays unverified.

1. Prepare one account using profile none into `$BENCH_WORK/denied`. Merge its manifest entry
   with one positive entry into a separate pool, assigning unique labels `denied-user` and
   `allowed-user` and resolving token/evidence paths against each original manifest directory.
   Use the same ns-00 Results list case with expected_by_client denied-user=deny. A successful
   response for denied-user invalidates the fixture, since inherited RBAC may grant access.
2. Prepare another none-profile account into `$BENCH_WORK/restricted`. Bind a test Role allowing
   only Results list in ns-00. Verify four cases: ns-00 Results list allowed, ns-00 Records list
   denied, ns-00 Result get denied, and ns-01 Results list denied.
3. Add a private file containing a deliberately invalid nonempty token and a pool entry with null
   identity plus intentionally_invalid=true. Resolve all its case expectations to invalid.
   Verify valid users continue working and invalid responses remain visible in raw metrics.

For the invalid-token cache experiment, a rejected API response alone is insufficient. Before
cold-state establishment, an operator permitted to create TokenReviews can verify the private
invalid token and retain only decision flags (never the echoed request or raw error text):

```bash
: "${BENCH_INVALID_TOKEN_FILE:?Set this to a private invalid-token file}"
jq -n --rawfile token "$BENCH_INVALID_TOKEN_FILE" '{
  apiVersion:"authentication.k8s.io/v1",kind:"TokenReview",
  spec:{token:($token|gsub("^\\s+|\\s+$";""))}
}' | kubectl --context "$BENCH_CONTEXT" create -f - -o json 2>/dev/null |
  jq '{authenticated:(.status.authenticated // false),
       has_error:((.status.error // "") != "")}' > "$BENCH_WORK/invalid-decision.json"
jq -e '.authenticated == false and .has_error == false' "$BENCH_WORK/invalid-decision.json"
```

A failed command or `has_error:true` cannot certify negative caching; select another known fixture
or record that branch unverified. Do not publish the TokenReview body. Once verified, append the
private-file entry to `ttl-pool.json` with ID `invalid-user`, null identity and
`intentionally_invalid:true`, and add `expected_by_client.invalid-user="invalid"` to the case.
The phase loop derives its count from the pool. Keep the invalid mapping and decision receipt
stable across experiments. The allowed/denied experiments remain valid without this optional
invalid-cache fixture; ordinary invalid-response isolation must still be tested separately.

Concrete restricted-role setup, using the account name from its prepared manifest:

```bash
"$BENCH_WORK/bench" users prepare --context "$BENCH_CONTEXT" \
  --namespace benchmark-users --profile none --count 1 --token-duration 2h \
  --output-dir "$BENCH_WORK/restricted"
RESTRICTED_SA=$(jq -r '.clients[0].identity.username | split(":")[-1]' \
  "$BENCH_WORK/restricted/pool.json")
kubectl --context "$BENCH_CONTEXT" -n ns-00 create role cache-validation-list \
  --verb=list --resource=results.results.tekton.dev
kubectl --context "$BENCH_CONTEXT" -n ns-00 create rolebinding cache-validation-list \
  --role=cache-validation-list --serviceaccount="benchmark-users:$RESTRICTED_SA"

RESULT_NAME=$(jq -r '.instances[0].result_name' "$BENCH_WORK/dataset.json")
jq -n --arg name "$RESULT_NAME" '{
  schema_version:"1",cases:[
    {id:"verify-fixture",operation:"get-result",name:$name,expected:"allow"}
  ]}' > "$BENCH_WORK/fixture-case.json"
"$BENCH_WORK/bench" query "${BENCH_COMMON[@]}" "${BENCH_CAPTURE[@]}" \
  --token "$BENCH_ADMIN" --access-cases "$BENCH_WORK/fixture-case.json" \
  --dataset-count 1000 --count 1 --concurrency 1 --transport grpc \
  --output "$BENCH_WORK/fixture-check.json"

jq -n --arg name "$RESULT_NAME" '{
  schema_version:"1",cases:[
    {id:"allowed-list",operation:"list-results",parent:"ns-00",expected:"allow"},
    {id:"denied-resource",operation:"list-records",parent:"ns-00/results/-",expected:"deny"},
    {id:"denied-verb",operation:"get-result",name:$name,expected:"deny"},
    {id:"denied-namespace",operation:"list-results",parent:"ns-01",expected:"deny"}
  ]}' > "$BENCH_WORK/isolation-cases.json"

"$BENCH_WORK/bench" query "${BENCH_COMMON[@]}" "${BENCH_CAPTURE[@]}" \
  --token-pool "$BENCH_WORK/restricted/pool.json" \
  --access-cases "$BENCH_WORK/isolation-cases.json" \
  --dataset-count 1000 --count 8 --concurrency 1 --transport both \
  --output "$BENCH_WORK/isolation.json"
```

Expected: eight completed client/case/transport pairs, response validation passed, and six raw
auth rejections (three denied cases on each transport). Change one expected deny to allow and
rerun with explicit expected-failure handling: the report must persist, validation must fail,
and the command must exit nonzero. The administrative fixture check must pass before this
matrix, outside cache measurements. A denied request cannot establish target existence because
authorization can happen before lookup. A 404 or server error is never an expected auth denial.

## Compatibility, lifecycle, and failure validation

- Repeat single-token query with the administrative token and no new options; retain legacy
  output behavior. Compare its dataset definition to the same seeded parameters.
- Run `load --verify` using the positive pool, count 1,000 and the same dataset parameters.
  Its first member must have records/list access. Loading existing data is an idempotency check,
  not a fresh-ingestion baseline; raw AlreadyExists values remain visible.
- Exercise store and mixed separately against fresh data states using the positive pool and
  matching namespace parameters. Do not run store then mixed over the same deterministic live
  identities and call the latter fresh ingestion.
- Cancel a diagnostic run: it must retain raw cancellation errors, incomplete work and coverage,
  stop clients, and preserve its available report without reporting a pass.
- Verify normal duration expiry reports completed execution with `duration-limit-reached`,
  while enclosing cancellation/deadline reports interruption with the corresponding reason.
  An unfinished case remains incomplete and an earlier mismatch remains failed. Count-bounded
  mixed-reader shutdown after writers finish reports `count-limit-reached`.
- Inject independent report-file, stdout, and send-log write failures in automated fixtures,
  including after interrupted/failed diagnostics. Require nonzero exit, preservation of other
  successful outputs, identical serialized report bytes where writes succeed, skipped optional
  load verification, and client cleanup. Output failure must not relabel measured execution.
- Reject duplicate tokens, empty files, duplicate labels, unknown case users, and explicit
  single-token/pool conflicts before any measured request.
- Verify `--dataset-count 1000` keeps the seeded hash unchanged for query counts 1,000, 2,000,
  and 4,000. Counts change workload repetitions, not data.
- Automated TLS fixture tests must cover every lifecycle/page credential, repeated-page-token
  failures, REST status/body cleanup, mismatched expectations, token-free errors/artifacts,
  partial preparation, and ownership/UID replacement failures.

Validate best-effort collection by directing a copy of the metrics configuration to an unavailable
local port, then running a known-good query. The workload report must survive with collection
unavailable and its ordinary response outcome intact:

```bash
jq '.sources[0].url="http://127.0.0.1:1/metrics" | .sources[0].timeout="1s"' \
  "$BENCH_WORK/metrics.json" > "$BENCH_WORK/metrics-unavailable.json"
"$BENCH_WORK/bench" query "${BENCH_COMMON[@]}" \
  --token-pool "$BENCH_WORK/users/pool.json" --access-cases "$BENCH_WORK/cases.json" \
  --metrics-config "$BENCH_WORK/metrics-unavailable.json" \
  --dataset-count 1000 --count 1000 --concurrency 8 --transport grpc \
  --output "$BENCH_WORK/metrics-unavailable-report.json"
jq -e '.validation.metrics_capture.status == "unavailable" and
  .validation.responses.status == "passed"' "$BENCH_WORK/metrics-unavailable-report.json"
```

Automated local fixtures additionally prove counter arithmetic, actual zero versus missing,
reset/process-change invalidation, label-scope mismatch, explicit timestamp rejection, safe errors,
TLS/auth handling, bounded time/body/series failures, before-only/after-only preservation, and
final capture after workload cancellation. No live cache hit assertion follows from these fixtures.

Required implementation checks:

```bash
go test -race -cover ./...
go build ./...
go vet ./...
make golangci-lint
gofmt -l test/performance/benchmark test/performance/report test/e2e/client
```

Check applicable shell scripts/examples with ShellCheck and bash -n. Existing unrelated failures
remain explicitly reported; they are not a reason to silently claim successful validation.

## Cleanup

Remove only the explicit manual isolation grants, then clean the prepared populations:

```bash
kubectl --context "$BENCH_CONTEXT" -n ns-00 delete \
  rolebinding/cache-validation-list role/cache-validation-list
"$BENCH_WORK/bench" users cleanup --context "$BENCH_CONTEXT" \
  --state "$BENCH_WORK/restricted/ownership.json"
"$BENCH_WORK/bench" users cleanup --context "$BENCH_CONTEXT" \
  --state "$BENCH_WORK/users/ownership.json"
"$BENCH_WORK/bench" users cleanup --context "$BENCH_CONTEXT" \
  --state "$BENCH_WORK/users/ownership.json"
```

Also clean `ttl-denied` and denied/other populations if created, using their ownership inventories.
Stop the local metrics forward after the last collection. Expected: owned account/RBAC resources and
credential files disappear; completed non-secret inventories remain; repeat cleanup succeeds;
namespaces, seed data, supplied pools, and unrelated artifacts are preserved. A partial-preparation
test must leave enough state for the same cleanup command. Preserve reports/evidence before
optionally deleting the disposable Kind cluster.
