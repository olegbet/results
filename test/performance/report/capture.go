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

// MetricSelector identifies exactly one public scalar series and its signal role.
type MetricSelector struct {
	ID     string            `json:"id"`
	Metric string            `json:"metric"`
	Type   string            `json:"type"`
	Labels map[string]string `json:"labels"`
	Signal string            `json:"signal"`
}

// MetricSource describes a source without its URL or private access configuration.
type MetricSource struct {
	ID               string           `json:"id"`
	InstanceID       string           `json:"instance_id"`
	ExclusiveTraffic bool             `json:"exclusive_traffic"`
	ScopeEvidenceRef string           `json:"scope_evidence_ref,omitempty"`
	Timeout          string           `json:"timeout"`
	Selectors        []MetricSelector `json:"selectors"`
}

// CaptureConfig publishes only declared scope, selected series and fixed bounds.
type CaptureConfig struct {
	Sources      []MetricSource `json:"sources"`
	MaxSources   int            `json:"max_sources"`
	MaxSelectors int            `json:"max_selectors_per_source"`
	MaxTimeout   string         `json:"max_timeout"`
	MaxBodyBytes int64          `json:"max_body_bytes"`
	MaxSeries    int            `json:"max_series"`
}

// MetricObservation distinguishes absent values from genuine numeric zero.
type MetricObservation struct {
	ID     string            `json:"id"`
	Metric string            `json:"metric"`
	Type   string            `json:"type"`
	Labels map[string]string `json:"labels"`
	Status string            `json:"status"`
	Value  *float64          `json:"value"`
	Reason *string           `json:"reason"`
}

// CaptureAttempt retains the actual collection window and selected observations.
type CaptureAttempt struct {
	Status           string              `json:"status"`
	StartedAt        string              `json:"started_at"`
	EndedAt          string              `json:"ended_at"`
	Reason           *string             `json:"reason"`
	HTTPStatus       int                 `json:"http_status,omitempty"`
	ProcessStartTime *float64            `json:"process_start_time_seconds"`
	Observations     []MetricObservation `json:"observations"`
}

// MetricDelta describes counter arithmetic independently of collection success.
type MetricDelta struct {
	ID     string   `json:"id"`
	Status string   `json:"status"`
	Delta  *float64 `json:"delta"`
	Reason *string  `json:"reason"`
}

// MetricSignal retains explicit availability for each named cache/review signal.
type MetricSignal struct {
	SelectorID *string `json:"selector_id"`
	Status     string  `json:"status"`
	Reason     *string `json:"reason"`
}

// HitRatio is available only for compatible genuine hit and miss counter deltas.
type HitRatio struct {
	Status string   `json:"status"`
	Value  *float64 `json:"value"`
	Reason *string  `json:"reason"`
}

// CapturedSource binds observations and derivation to one declared server instance.
type CapturedSource struct {
	ID         string                  `json:"id"`
	InstanceID string                  `json:"instance_id"`
	Before     *CaptureAttempt         `json:"before"`
	After      *CaptureAttempt         `json:"after"`
	Derived    []MetricDelta           `json:"derived"`
	Signals    map[string]MetricSignal `json:"signals"`
	HitRatios  map[string]HitRatio     `json:"hit_ratios"`
}

// MetricCapture contains sanitized observations rather than unfiltered scrape bodies.
type MetricCapture struct {
	Sources []CapturedSource `json:"sources"`
}

// CaptureStatus describes only completeness of configured sample collection.
type CaptureStatus struct {
	Status      string   `json:"status"`
	ReasonCodes []string `json:"reason_codes"`
}
