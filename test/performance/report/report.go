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

// Package report defines the machine-readable benchmark report schema emitted by
// the benchmark and consumed by the compare tool (Story 02). It converts a
// metrics.Snapshot into a stable, versioned JSON document annotated with run
// metadata (git commit, dataset version, tier, backend).
package report

import (
	"encoding/json"
	"io"
	"time"

	"github.com/tektoncd/results/test/performance/metrics"
)

// SchemaVersion identifies the report format; bump on any breaking field change.
const SchemaVersion = "1"

// Report is the top-level benchmark artifact.
type Report struct {
	SchemaVersion string      `json:"schema_version"`
	Meta          Meta        `json:"meta"`
	Config        Config      `json:"config"`
	Metrics       Metrics     `json:"metrics"`
	Validation    *Validation `json:"validation,omitempty"`
	Scenario      *Scenario   `json:"scenario,omitempty"`
}

// Meta captures the provenance needed to compare two runs meaningfully.
type Meta struct {
	GitCommit      string `json:"git_commit"`
	RunID          string `json:"run_id,omitempty"`
	GitDirty       bool   `json:"git_dirty"`
	DatasetVersion string `json:"dataset_version"`
	DatasetHash    string `json:"dataset_hash"`
	Tier           string `json:"tier"`
	Mode           string `json:"mode"`
	StartedAt      string `json:"started_at"`
	DurationMS     int64  `json:"duration_ms"`
	APIServerAddr  string `json:"api_server_addr"`
	DBBackend      string `json:"db_type"`
	ServerImage    string `json:"server_image"`
}

// Config records the knobs that shaped the run so results are reproducible.
type Config struct {
	Count          int            `json:"count"`
	Concurrency    int            `json:"concurrency"`
	DurationSec    int            `json:"duration_sec"`
	Transport      string         `json:"transport"`
	ReadRatio      int            `json:"read_ratio"`
	WriteRatio     int            `json:"write_ratio"`
	Dataset        string         `json:"dataset"`
	Seed           int64          `json:"seed"`
	DatasetCount   *int           `json:"dataset_count,omitempty"`
	ClientPool     *ClientPool    `json:"client_pool,omitempty"`
	MetricsCapture *CaptureConfig `json:"metrics_capture,omitempty"`
}

// Metrics is the aggregate performance result.
type Metrics struct {
	ThroughputPerSec float64             `json:"throughput_per_sec"`
	TotalOps         int64               `json:"total_ops"`
	TotalErrors      int64               `json:"total_errors"`
	ByOp             map[string]OpReport `json:"by_op"`
	SeedLoad         *SeedLoadMetrics    `json:"seed_load,omitempty"`
	Clients          []ClientMetrics     `json:"clients,omitempty"`
	Capture          *MetricCapture      `json:"capture,omitempty"`
}

// SeedLoadMetrics summarizes write errors across attempted seed instances.
// Each instance counts once if any write for it failed.
type SeedLoadMetrics struct {
	TotalInstances   int     `json:"total_instances"`
	FailedInstances  int     `json:"failed_instances"`
	ErrorRatePercent float64 `json:"error_rate_percent"`
}

// OpReport is the per-operation summary in report units (milliseconds).
type OpReport struct {
	Count     int64            `json:"count"`
	Errors    int64            `json:"errors"`
	ExitCodes map[string]int64 `json:"exit_codes"`
	P50MS     float64          `json:"p50_ms"`
	P90MS     float64          `json:"p90_ms"`
	P99MS     float64          `json:"p99_ms"`
	MinMS     float64          `json:"min_ms"`
	MaxMS     float64          `json:"max_ms"`
	MeanMS    float64          `json:"mean_ms"`
}

// New assembles a Report from run metadata, config, and a metrics snapshot.
func New(meta Meta, cfg Config, snap metrics.Snapshot) *Report {
	meta.DurationMS = snap.Duration.Milliseconds()
	if meta.StartedAt == "" && !snap.Start.IsZero() {
		meta.StartedAt = snap.Start.UTC().Format(time.RFC3339)
	}
	return &Report{
		SchemaVersion: SchemaVersion,
		Meta:          meta,
		Config:        cfg,
		Metrics:       buildMetrics(snap),
	}
}

// buildMetrics converts the metrics snapshot into report units.
func buildMetrics(snap metrics.Snapshot) Metrics {
	m := Metrics{
		TotalOps:    snap.TotalOps,
		TotalErrors: snap.TotalError,
		ByOp:        make(map[string]OpReport, len(snap.Ops)),
	}
	if secs := snap.Duration.Seconds(); secs > 0 {
		m.ThroughputPerSec = float64(snap.TotalOps) / secs
	}
	for _, op := range snap.Ops {
		m.ByOp[op.Name] = OpReport{
			Count:     op.Total,
			Errors:    op.ErrorTotal,
			ExitCodes: op.Errors,
			P50MS:     ms(op.Latency.P50),
			P90MS:     ms(op.Latency.P90),
			P99MS:     ms(op.Latency.P99),
			MinMS:     ms(op.Latency.Min),
			MaxMS:     ms(op.Latency.Max),
			MeanMS:    ms(op.Latency.Mean),
		}
	}
	return m
}

// ms converts a duration to fractional milliseconds.
func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// Write emits the report as indented JSON with a trailing newline.
func Write(w io.Writer, r *Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// Read decodes a report from JSON.
func Read(r io.Reader) (*Report, error) {
	rep := &Report{}
	if err := json.NewDecoder(r).Decode(rep); err != nil {
		return nil, err
	}
	return rep, nil
}

// ClientPool describes ordered credentials without publishing credential material.
type ClientPool struct {
	PopulationID           string `json:"population_id"`
	ConfiguredClients      int    `json:"configured_clients"`
	PoolFingerprint        string `json:"pool_fingerprint"`
	Selection              string `json:"selection"`
	DistinctUsers          int    `json:"distinct_users"`
	SupportedDistinctUsers int    `json:"supported_distinct_users"`
	DeclaredUsers          int    `json:"declared_users"`
	ProvisionedUsers       int    `json:"provisioned_users"`
	VerifiedUsers          int    `json:"verified_users"`
}

// OpCounts retains per-client counts and status codes without latency samples.
type OpCounts struct {
	Count     int64            `json:"count"`
	Errors    int64            `json:"errors"`
	ExitCodes map[string]int64 `json:"exit_codes"`
}

// ClientMetrics attributes every observation to its logical operation's client.
type ClientMetrics struct {
	ID                   string              `json:"id"`
	LogicalStarted       int64               `json:"logical_started"`
	LogicalCompleted     int64               `json:"logical_completed"`
	TotalOps             int64               `json:"total_ops"`
	TotalErrors          int64               `json:"total_errors"`
	APIOps               int64               `json:"api_ops"`
	APISuccesses         int64               `json:"api_successes"`
	LocalErrors          int64               `json:"local_errors"`
	ExpectedRejections   int64               `json:"expected_rejections"`
	UnexpectedGrants     int64               `json:"unexpected_grants"`
	UnexpectedRejections int64               `json:"unexpected_rejections"`
	UnexpectedErrors     int64               `json:"unexpected_errors"`
	ByOp                 map[string]OpCounts `json:"by_op"`
}

// Execution distinguishes workload bounds from enclosing cancellation.
type Execution struct {
	Status      string   `json:"status"`
	ReasonCodes []string `json:"reason_codes"`
}

// MeasurementWindow records the actual high-precision workload window.
type MeasurementWindow struct {
	StartedAt string `json:"started_at"`
	EndedAt   string `json:"ended_at"`
}

// Coverage distinguishes dispatch, successful participation and complete matrices.
type Coverage struct {
	ExercisedClients  int    `json:"exercised_clients"`
	SuccessfulClients int    `json:"successful_clients"`
	RequiredPairs     int    `json:"required_pairs,omitempty"`
	AttemptedPairs    int    `json:"attempted_pairs,omitempty"`
	CompletedPairs    int    `json:"completed_pairs,omitempty"`
	Status            string `json:"status"`
}

// Responses summarizes explicit expectation checks independently of raw errors.
type Responses struct {
	Status     string `json:"status"`
	Mismatches int64  `json:"mismatches"`
}

// CaseResult summarizes all repetitions of one client/case/transport assignment.
type CaseResult struct {
	ClientID         string `json:"client_id"`
	CaseID           string `json:"case_id"`
	Transport        string `json:"transport"`
	Expected         string `json:"expected"`
	LogicalStarted   int64  `json:"logical_started"`
	LogicalCompleted int64  `json:"logical_completed"`
	Mismatches       int64  `json:"mismatches"`
}

// Validation keeps execution, coverage and response outcomes independent.
type Validation struct {
	Execution         Execution         `json:"execution"`
	MeasurementWindow MeasurementWindow `json:"measurement_window"`
	Coverage          *Coverage         `json:"coverage,omitempty"`
	Responses         Responses         `json:"responses"`
	CaseResults       []CaseResult      `json:"case_results,omitempty"`
	MetricsCapture    *CaptureStatus    `json:"metrics_capture,omitempty"`
}

// CacheConfig records decision TTLs declared or independently verified by operators.
type CacheConfig struct {
	Enabled          bool   `json:"enabled"`
	PositiveTTL      string `json:"positive_ttl"`
	NegativeTTL      string `json:"negative_ttl"`
	CapacityPerCache int    `json:"capacity_per_cache"`
	Provenance       string `json:"provenance"`
	EvidenceRef      string `json:"evidence_ref,omitempty"`
}

// Environment records comparison controls without fetching their evidence.
type Environment struct {
	ServerImage           string  `json:"server_image"`
	ServerRevision        string  `json:"server_revision"`
	Replicas              int     `json:"replicas"`
	SelectedPod           string  `json:"selected_pod"`
	Routing               string  `json:"routing"`
	AuthenticationEnabled bool    `json:"authentication_enabled"`
	K8sQPS                float64 `json:"k8s_qps"`
	K8sBurst              int     `json:"k8s_burst"`
	DBType                string  `json:"db_type"`
	ConfigurationRef      string  `json:"configuration_ref"`
	Provenance            string  `json:"provenance"`
	EvidenceRef           string  `json:"evidence_ref,omitempty"`
}

// Controls records required evidence references and operator workload declarations.
type Controls struct {
	WorkingSet       string `json:"working_set"`
	ExclusiveTraffic bool   `json:"exclusive_traffic"`
	ColdStateRef     string `json:"cold_state_ref"`
	PriorReportRef   string `json:"prior_report_ref"`
	ColdReportRef    string `json:"cold_report_ref,omitempty"`
	Provenance       string `json:"provenance"`
	EvidenceRef      string `json:"evidence_ref,omitempty"`
}

// Scenario retains comparison context; cache correctness is assessed separately.
type Scenario struct {
	SchemaVersion          string       `json:"schema_version,omitempty"`
	ScenarioID             string       `json:"scenario_id,omitempty"`
	Phase                  string       `json:"phase,omitempty"`
	Cache                  *CacheConfig `json:"cache,omitempty"`
	Environment            *Environment `json:"environment,omitempty"`
	Control                *Controls    `json:"control,omitempty"`
	AccessCasesFingerprint string       `json:"access_cases_fingerprint,omitempty"`
	CacheAssessment        string       `json:"cache_assessment,omitempty"`
}
