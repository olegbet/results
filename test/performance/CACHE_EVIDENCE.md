# Cache Evidence Validation Guide

This optional procedure is for a disposable Kind environment after feature implementation.
It changes test-cluster configuration, not Results server code. It has not been live-executed
during planning. Use Docker-backed Kind for the commands below; another provider requires its
equivalent node-file access command.

The main [quickstart](./QUICKSTART_MULTIUSER.md) initializes `BENCH_WORK` and a private `KUBECONFIG`
before this procedure. Do this instead of the ordinary cluster-creation script, before installing
Results. Do not reuse a live cluster name.

## Create a cluster with Metadata-only audit

```bash
set -euo pipefail
mkdir -p "$BENCH_WORK/audit/logs"
cat > "$BENCH_WORK/audit/policy.yaml" <<'YAML'
apiVersion: audit.k8s.io/v1
kind: Policy
omitStages:
  - RequestReceived
rules:
  - level: Metadata
    users:
      - system:serviceaccount:tekton-pipelines:api
    verbs:
      - create
    resources:
      - group: authentication.k8s.io
        resources: [tokenreviews]
      - group: authorization.k8s.io
        resources: [subjectaccessreviews]
  - level: None
YAML

cat > "$BENCH_WORK/kind-audit.yaml" <<YAML
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    image: kindest/node:v1.35.0@sha256:452d707d4862f52530247495d180205e029056831160e22870e37e3f6c1ac31f
    extraMounts:
      - hostPath: $BENCH_WORK/audit
        containerPath: /benchmark-audit
    kubeadmConfigPatches:
      - |
        apiVersion: kubeadm.k8s.io/v1beta4
        kind: ClusterConfiguration
        apiServer:
          extraArgs:
            - name: audit-policy-file
              value: /etc/kubernetes/audit-policy.yaml
            - name: audit-log-path
              value: /var/log/kubernetes/audit.log
            - name: audit-log-mode
              value: blocking
          extraVolumes:
            - name: audit-policy
              hostPath: /benchmark-audit/policy.yaml
              mountPath: /etc/kubernetes/audit-policy.yaml
              readOnly: true
              pathType: File
            - name: audit-log
              hostPath: /benchmark-audit/logs
              mountPath: /var/log/kubernetes
              readOnly: false
              pathType: DirectoryOrCreate
  - role: worker
    image: kindest/node:v1.35.0@sha256:452d707d4862f52530247495d180205e029056831160e22870e37e3f6c1ac31f
    extraPortMappings:
      - containerPort: 30080
        hostPort: 8080
YAML

kind create cluster --name tekton-results --config "$BENCH_WORK/kind-audit.yaml" --wait 120s
```

The policy's caller must match the Results API pod service account. Verify it from the actual
deployment before collecting evidence. This setup retains the repository's node image/topology.
The blocking audit backend makes collection boundaries meaningful but adds control-plane
overhead: use the same audit configuration for every compared phase and record it.

## Bracket one finite workload

Use one controlled API process and no other Results traffic during collection. Seed verification,
population setup, readiness checks, and port-forward startup must precede the bookmark.
The automatic collector's before/after calls are inside the whole-command bookmark interval.
For this guide, use the separate Results metrics listener pinned to the same pod: it does not run
TokenReview/SAR. Verify this and the caller filter before accepting counts. For a protected proxy
or other source that triggers reviews, separately identify/exclude collector activity with complete
evidence or mark inference unverified; never guess-subtract scrape calls. Before collection must
not warm the tested cache. Counter intervals are wider than workload timing and require their own
scope checks. See [metrics capture](metrics.md#capture-lifecycle-and-bounds).

Before the benchmark command:

```bash
AUDIT_NODE=tekton-results-control-plane
AUDIT_LOG=/benchmark-audit/logs/audit.log
AUDIT_FIRST_INODE=$(docker exec "$AUDIT_NODE" stat -c '%i' "$AUDIT_LOG")
AUDIT_BEFORE=$(docker exec "$AUDIT_NODE" sh -c 'wc -l < "$1"' sh "$AUDIT_LOG")
AUDIT_BEFORE_PROCESS=$(kubectl --context "$BENCH_CONTEXT" -n tekton-pipelines \
  get pod "$BENCH_POD" -o json | jq -ce '{
    pod_uid:.metadata.uid,
    api:[.status.containerStatuses[] | select(.name=="api") |
      {container_id:.containerID,restart_count:.restartCount,
       started_at:.state.running.startedAt}]
  } | if (.api|length)==1 and .api[0].started_at!=null then .
      else error("API process is not running") end')
AUDIT_START=$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)
```

Run the finite benchmark command and retain its report as `RUN_REPORT`. Do not run other
Results requests between the bookmarks. Then collect only metadata projection:

```bash
AUDIT_END=$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)
AUDIT_AFTER=$(docker exec "$AUDIT_NODE" sh -c 'wc -l < "$1"' sh "$AUDIT_LOG")
AUDIT_LAST_INODE=$(docker exec "$AUDIT_NODE" stat -c '%i' "$AUDIT_LOG")
test "$AUDIT_FIRST_INODE" = "$AUDIT_LAST_INODE"
test "$AUDIT_AFTER" -ge "$AUDIT_BEFORE"
AUDIT_AFTER_PROCESS=$(kubectl --context "$BENCH_CONTEXT" -n tekton-pipelines \
  get pod "$BENCH_POD" -o json | jq -ce '{
    pod_uid:.metadata.uid,
    api:[.status.containerStatuses[] | select(.name=="api") |
      {container_id:.containerID,restart_count:.restartCount,
       started_at:.state.running.startedAt}]
  } | if (.api|length)==1 and .api[0].started_at!=null then .
      else error("API process is not running") end')
test "$AUDIT_BEFORE_PROCESS" = "$AUDIT_AFTER_PROCESS"

docker exec "$AUDIT_NODE" cat "$AUDIT_LOG" |
  awk -v first="$AUDIT_BEFORE" -v last="$AUDIT_AFTER" 'NR > first && NR <= last' |
  jq -c 'select(.stage == "ResponseComplete" and .verb == "create"
    and .user.username == "system:serviceaccount:tekton-pipelines:api"
    and ((.objectRef.apiGroup == "authentication.k8s.io"
      and .objectRef.resource == "tokenreviews")
      or (.objectRef.apiGroup == "authorization.k8s.io"
      and .objectRef.resource == "subjectaccessreviews")))
    | {auditID, stageTimestamp, caller: .user.username,
       group: .objectRef.apiGroup, resource: .objectRef.resource,
       code: .responseStatus.code}' |
  jq -s 'unique_by(.auditID)' > "$RUN_REPORT.audit.json"

jq '{
  tokenreviews: ([.[] | select(.resource == "tokenreviews")] | length),
  subjectaccessreviews: ([.[] | select(.resource == "subjectaccessreviews")] | length),
  failed_reviews: ([.[] | select(.code < 200 or .code >= 300)] | length)
}' "$RUN_REPORT.audit.json" > "$RUN_REPORT.review-counts.json"

jq -n --arg start "$AUDIT_START" --arg end "$AUDIT_END" \
  --arg node "$AUDIT_NODE" --arg inode "$AUDIT_FIRST_INODE" \
  --argjson before "$AUDIT_BEFORE" --argjson after "$AUDIT_AFTER" \
  --argjson process_before "$AUDIT_BEFORE_PROCESS" --argjson process_after "$AUDIT_AFTER_PROCESS" \
  '{start:$start,end:$end,node:$node,inode:$inode,
    before_line:$before,after_line:$after,
    process_before:$process_before,process_after:$process_after}' > "$RUN_REPORT.audit-window.json"

sha256sum "$RUN_REPORT" "$RUN_REPORT.audit.json"
```

A changed API process, changed inode, shrinking file, dropped audit events, unrelated traffic,
or a canceled run
invalidates an inference from these counts. Record bookmarks, timestamps, source, and collection
completeness with the counts. Metadata does not show which bearer token was reviewed, nor the
allowed/denied result inside a successful review response. Do not infer per-user cache hits
or distinguish authentication from permission failure from audit metadata alone.

Strict shell mode makes failed bookmarks, process checks, or collector pipelines stop collection.
Treat any such failure as unverified evidence; do not resume with an empty file and call it a warm
cache hit. Before accepting zero warm reviews, require nonzero cold/disabled calibration from the
same verified caller, policy, and audit source, linked through the corresponding assessment.
Compare process receipts across cold, warm, and expiry phases as well as within each phase:
a container restart inside the same pod also invalidates reuse/expiry inference.

## Assess the four conditions

Use identical dataset, pool fingerprint, cases, count, transport, image, capacity, and routing.
The effective setting under evaluation is the intentional difference.

Require the [sanitized environment receipt](../quickstart.md#record-environment-controls)
referenced by each run context. Verify effective database configuration and client/API/database
resource capacity, recheck them before each phase, and compare with prior receipts. Cache capacity
is a separate control from CPU/memory/storage capacity. Missing or unverified receipt values
leave the assessment unverified. Retain each receipt unchanged and bind its reference and
SHA-256 as `controls.environment_configuration_ref` and
`controls.environment_configuration_sha256`; a hash alone does not prove the settings.

For the quickstart's absolute reference, compute the exact receipt hash for the assessment:

```bash
BENCH_ENV_RECEIPT=$(jq -er '.scenario.environment.configuration_ref |
  select(type == "string" and length > 0)' "$RUN_REPORT")
sha256sum "$BENCH_ENV_RECEIPT"
```

For a relative reference, resolve it against the original run-context file directory, not the
report directory. Include the receipt hash with the report/evidence hashes when assembling the
operator assessment. Do not mark comparison controls verified until their values have been checked.

Use distinct stable identities/tokens for each column, a fixed access tuple, and unchanged
permissions. TR means TokenReview; SAR means SubjectAccessReview. Counts are population-level
for the isolated cohort, not per-user hit estimates. Measure and bracket each cohort separately;
mixed-cohort aggregate counts cannot establish each column because incorrect activity in one
cohort can cancel missing activity in another.

| Phase | Allowed user | Authenticated but denied user | Definitively unauthenticated credential |
| --- | --- | --- | --- |
| Disabled / cold | TR + SAR activity | TR + SAR activity | TR activity only |
| Warm before both TTLs | No additional reviews | No additional reviews | No additional reviews |
| Positive expired, negative live | TR + SAR resume | TR resumes; denied SAR reused | Negative authn reused; no SAR |
| Negative expired, positive live | Both decisions reused | SAR resumes; authn reused | TR resumes; no SAR |

Positive TTL governs successful authentication and allowed authorization. Negative TTL governs
unauthenticated and denied decisions. An invalid credential does not reach authorization. Keep
all valid tokens alive through the experiment: only successful authn caching is capped by JWT
expiry, not every negative entry. Transient review errors are not cacheable negative decisions.
A public rejection or a Metadata-level successful review alone does not establish a definitive
negative authentication fixture; require safe decision-only provenance showing `Authenticated=false`
and empty `Status.Error` outside measurement. For the authenticated-denied cohort, establish
successful authentication and SAR `Allowed=false` with empty `EvaluationError` for the exact
identity (including groups/extra) and access tuple. Preserve only non-secret fixture attributes
and decision/error-present flags, never raw review requests or error text. Without definitive
fixture evidence, response checks remain useful but that cohort's cache conclusion is unverified.

For cold interval [S,E], using `validation.measurement_window`:

- Complete warm access before `S + shortTTL - margin`.
- Begin short-expiry access after `E + shortTTL + margin`.
- Complete it before `S + longTTL - margin`.
- Ensure actual credential expiry is later than the complete experiment.
- Keep the working set below capacity and the same pod process throughout the sequence.

Use two fresh-process experiments: short positive/long negative TTL, then short negative/long
positive TTL. Assess allowed and authenticated-denied cohorts separately in both, plus the invalid
cohort when a definitive fixture is available; otherwise leave that branch unverified. If the inequalities do not fit,
increase TTL separation or reduce the functional-test population. Automatic before/after capture
also consumes wall time even though it is excluded from workload latency, so allow for it. Do not infer a cache failure
from an expired warm entry. Do not assume concurrent cold requests are coalesced.

After verifying controls, assemble `<report>.cache-assessment.json` according to the
[artifact contract](artifacts.md#post-run-cache-assessment). Use the collected counts, report and
evidence hashes, scenario/run IDs, bookmarks, and concrete reasons. Start with status unverified;
record passed only with complete evidence matching the phase, or failed with complete
contradicting evidence. This is an explicit operator assessment, not an automatic benchmark pass.

For expected deny/invalid cases, first establish the fixture's actual behavior with caching
disabled. Definitive permission denials can be cached; transient review errors must not be
mistaken for cached negative decisions. An unexpected response already fails response validation,
independently of what review counts suggest.

## Alternatives and limits

A controlled environment may supply existing Kubernetes review counters instead. Retain before/
after samples, resource/verb labels, reset/completeness checks, isolation controls, and the same
window binding. These counters normally identify neither bearer user nor Results caller, so
background traffic must be excluded or independently accounted for.

Automatic snapshots may provide scoped review-counter deltas or genuine direct cache counters
when exposed by the deployment. Bind the immutable report hash plus source/selector IDs using
the [assessment contract](artifacts.md#post-run-cache-assessment). Verify exact metric semantics,
unchanged process identity, compatible labels, and no collector/background traffic contamination.
Missing direct counters are unavailable, not zero; supporting gRPC/CPU/memory metrics alone
cannot prove a hit. Do not treat incomplete, reset, or mismatched snapshots as valid evidence.

If no adequate evidence source is available, keep the benchmark's cache assessment unverified.
Latency/throughput remain useful measurements but cannot certify cache hit/miss/expiry behavior.
