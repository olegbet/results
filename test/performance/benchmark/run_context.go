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

package main

import (
	"fmt"
	"time"

	"github.com/tektoncd/results/test/performance/report"
)

type cacheContextInput struct {
	report.CacheConfig
	Enabled *bool `json:"enabled"`
}
type controlContextInput struct {
	report.Controls
	ExclusiveTraffic *bool `json:"exclusive_traffic"`
}

type runContextInput struct {
	SchemaVersion string               `json:"schema_version"`
	ScenarioID    string               `json:"scenario_id"`
	Phase         string               `json:"phase"`
	Cache         *cacheContextInput   `json:"cache"`
	Environment   *report.Environment  `json:"environment"`
	Control       *controlContextInput `json:"control"`
}

func loadRunContext(path string, cfg *RunConfig) (*report.Scenario, error) {
	var in runContextInput
	if err := readStrictJSON(path, &in); err != nil {
		return nil, fmt.Errorf("run context: %w", err)
	}
	if in.SchemaVersion != "1" || !publicID.MatchString(in.ScenarioID) || in.Cache == nil || in.Environment == nil || in.Control == nil {
		return nil, fmt.Errorf("run context: invalid-header")
	}
	if in.Cache.Enabled == nil || in.Control.ExclusiveTraffic == nil {
		return nil, fmt.Errorf("run context: missing-required-field")
	}
	c, e, k := &in.Cache.CacheConfig, in.Environment, &in.Control.Controls
	c.Enabled = *in.Cache.Enabled
	k.ExclusiveTraffic = *in.Control.ExclusiveTraffic
	switch in.Phase {
	case "disabled":
		if c.Enabled {
			return nil, fmt.Errorf("run context: inconsistent-cache")
		}
	case "cold", "warm", "after-positive-expiry", "after-negative-expiry":
		if !c.Enabled || k.ColdStateRef == "" {
			return nil, fmt.Errorf("run context: missing-cache-control")
		}
	default:
		return nil, fmt.Errorf("run context: invalid-phase")
	}
	if in.Phase != "cold" && in.Phase != "disabled" && (k.PriorReportRef == "" || k.ColdReportRef == "") {
		return nil, fmt.Errorf("run context: missing-prior-cold-reference")
	}
	for _, ttl := range []string{c.PositiveTTL, c.NegativeTTL} {
		duration, err := time.ParseDuration(ttl)
		if err != nil || duration <= 0 {
			return nil, fmt.Errorf("run context: invalid-decision-ttl")
		}
	}
	if c.CapacityPerCache <= 0 || !e.AuthenticationEnabled || e.ServerImage == "" || e.ServerRevision == "" || e.Replicas <= 0 || e.SelectedPod == "" || e.Routing == "" || e.K8sQPS <= 0 || e.K8sBurst <= 0 || e.DBType == "" || e.ConfigurationRef == "" || k.WorkingSet == "" {
		return nil, fmt.Errorf("run context: missing-environment-control")
	}
	for _, p := range []struct{ provenance, evidence string }{{c.Provenance, c.EvidenceRef}, {e.Provenance, e.EvidenceRef}, {k.Provenance, k.EvidenceRef}} {
		if p.provenance != "operator-declared" && p.provenance != "verified" {
			return nil, fmt.Errorf("run context: invalid-provenance")
		}
		if p.provenance == "verified" && p.evidence == "" {
			return nil, fmt.Errorf("run context: missing-verified-evidence")
		}
	}
	if e.ServerImage != cfg.ServerImage || e.DBType != cfg.DBBackend {
		return nil, fmt.Errorf("run context: CLI metadata conflict")
	}
	return &report.Scenario{SchemaVersion: in.SchemaVersion, ScenarioID: in.ScenarioID, Phase: in.Phase, Cache: c, Environment: e, Control: k, CacheAssessment: "unverified"}, nil
}
