/*
Copyright 2026 The Tekton Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package report

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/tektoncd/results/test/performance/metrics"
)

func sampleSnapshot() metrics.Snapshot {
	m := metrics.NewMetricSet()
	m.Start()
	for i := 1; i <= 100; i++ {
		m.Observe("create_record", time.Duration(i)*time.Millisecond, nil)
	}
	m.Observe("create_record", 5*time.Millisecond, status.Error(codes.Unavailable, "down"))
	m.Stop()
	snap := m.Snapshot()
	// Pin a deterministic duration so throughput is stable in assertions.
	snap.Duration = 2 * time.Second
	return snap
}

func TestNewReportShape(t *testing.T) {
	snap := sampleSnapshot()
	meta := Meta{GitCommit: "abc123", DatasetVersion: "seed-small-v1", Tier: "tier1", Mode: "store", DBBackend: "local"}
	cfg := Config{Count: 100, Concurrency: 8, Transport: "grpc", Seed: 42}

	r := New(meta, cfg, snap)

	if r.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion = %q, want %q", r.SchemaVersion, SchemaVersion)
	}
	if r.Meta.DurationMS != 2000 {
		t.Errorf("DurationMS = %d, want 2000", r.Meta.DurationMS)
	}
	op, ok := r.Metrics.ByOp["create_record"]
	if !ok {
		t.Fatal("missing create_record op report")
	}
	if op.Count != 101 {
		t.Errorf("Count = %d, want 101", op.Count)
	}
	if op.Errors != 1 || op.ExitCodes["Unavailable"] != 1 {
		t.Errorf("errors = %d, codes = %v", op.Errors, op.ExitCodes)
	}
	// 101 ops over 2s.
	if want := 101.0 / 2.0; r.Metrics.ThroughputPerSec != want {
		t.Errorf("ThroughputPerSec = %f, want %f", r.Metrics.ThroughputPerSec, want)
	}
}

func TestReportRoundTrip(t *testing.T) {
	r := New(
		Meta{GitCommit: "deadbeef", Tier: "tier1", Mode: "mixed"},
		Config{Count: 10, Concurrency: 2},
		sampleSnapshot(),
	)

	var buf bytes.Buffer
	if err := Write(&buf, r); err != nil {
		t.Fatalf("Write() = %v", err)
	}
	got, err := Read(&buf)
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}
	if diff := cmp.Diff(r, got); diff != "" {
		t.Errorf("round-trip mismatch (-want +got):\n%s", diff)
	}
}

func TestReportRequiredKeys(t *testing.T) {
	r := New(Meta{Mode: "query"}, Config{}, sampleSnapshot())
	var buf bytes.Buffer
	if err := Write(&buf, r); err != nil {
		t.Fatalf("Write() = %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(buf.Bytes(), &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"schema_version", "meta", "config", "metrics"} {
		if _, ok := generic[key]; !ok {
			t.Errorf("report missing top-level key %q", key)
		}
	}
	metricsObj := generic["metrics"].(map[string]any)
	for _, key := range []string{"throughput_per_sec", "total_ops", "total_errors", "by_op"} {
		if _, ok := metricsObj[key]; !ok {
			t.Errorf("metrics missing key %q", key)
		}
	}
}

func TestReportSeedLoadMetrics(t *testing.T) {
	for _, tt := range []struct {
		name    string
		summary *SeedLoadMetrics
	}{
		{name: "other mode"},
		{name: "empty load", summary: &SeedLoadMetrics{}},
		{name: "successful load", summary: &SeedLoadMetrics{TotalInstances: 100}},
		{name: "one percent errors", summary: &SeedLoadMetrics{TotalInstances: 100, FailedInstances: 1, ErrorRatePercent: 1}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mode := "load"
			if tt.summary == nil {
				mode = "query"
			}
			r := New(Meta{Mode: mode}, Config{}, sampleSnapshot())
			r.Metrics.SeedLoad = tt.summary
			var buf bytes.Buffer
			if err := Write(&buf, r); err != nil {
				t.Fatal(err)
			}
			var doc struct {
				Metrics map[string]json.RawMessage `json:"metrics"`
			}
			if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
				t.Fatal(err)
			}
			seedJSON, present := doc.Metrics["seed_load"]
			if tt.summary == nil {
				if present {
					t.Fatal("non-load report contains seed_load metrics")
				}
				return
			}
			var got map[string]float64
			if err := json.Unmarshal(seedJSON, &got); err != nil {
				t.Fatalf("decoding seed_load metrics: %v", err)
			}
			want := map[string]float64{
				"total_instances":    float64(tt.summary.TotalInstances),
				"failed_instances":   float64(tt.summary.FailedInstances),
				"error_rate_percent": tt.summary.ErrorRatePercent,
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("seed_load JSON mismatch (-want +got):\n%s", diff)
			}
			decoded, err := Read(&buf)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(r, decoded); diff != "" {
				t.Errorf("load report round-trip mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestLegacyAndExtendedReportCompatibility(t *testing.T) {
	legacy := New(Meta{}, Config{}, sampleSnapshot())
	var buf bytes.Buffer
	if err := Write(&buf, legacy); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["validation"]; ok {
		t.Fatal("legacy validation must be omitted")
	}
	legacy.Validation = &Validation{Execution: Execution{Status: "completed", ReasonCodes: []string{"count-limit-reached"}}}
	legacy.Config.ClientPool = &ClientPool{PopulationID: "fixture", ConfiguredClients: 1000}
	buf.Reset()
	if err := Write(&buf, legacy); err != nil {
		t.Fatal(err)
	}
	got, err := Read(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if got.Validation.Execution.Status != "completed" || got.Config.ClientPool.ConfiguredClients != 1000 || got.Metrics.ByOp["create_record"].ExitCodes["Unavailable"] != 1 {
		t.Fatal("additive report lost legacy data or extensions")
	}
}

func TestMetricProjectionPreservesLegacyAndUnavailable(t *testing.T) {
	rep := New(Meta{}, Config{}, sampleSnapshot())
	rep.Config.MetricsCapture = &CaptureConfig{Sources: []MetricSource{{ID: "api", InstanceID: "pod", ScopeEvidenceRef: "public-scope.json", Selectors: []MetricSelector{{ID: "hits", Metric: "test_hits_total", Type: "counter", Labels: map[string]string{}, Signal: "authn_hits"}}}}}
	rep.Metrics.Capture = &MetricCapture{Sources: []CapturedSource{{ID: "api", InstanceID: "pod", Before: &CaptureAttempt{Status: "available", Observations: []MetricObservation{{ID: "hits", Status: "unavailable"}}}}}}
	rep.Validation = &Validation{MetricsCapture: &CaptureStatus{Status: "partial"}}
	var buf bytes.Buffer
	if err := Write(&buf, rep); err != nil {
		t.Fatal(err)
	}
	got, err := Read(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metrics.Capture.Sources[0].Before.Observations[0].Value != nil || got.Validation.MetricsCapture.Status != "partial" || got.Metrics.ByOp["create_record"].ExitCodes["Unavailable"] != 1 {
		t.Fatal("lost metric availability or legacy fields")
	}
	for _, private := range []string{"bearer_token_file", "ca_file", "authorization", "\"url\""} {
		if bytes.Contains(buf.Bytes(), []byte(private)) {
			t.Fatal("private collector field in report")
		}
	}
}
