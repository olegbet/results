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
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func metricsFixture(t *testing.T, url string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "metrics.json")
	doc := map[string]any{"schema_version": "1", "sources": []any{map[string]any{"id": "api", "url": url, "instance_id": "fixture-process", "exclusive_traffic": true, "scope_evidence_ref": "scope.json", "timeout": "2s", "selectors": []any{map[string]any{"id": "hits", "metric": "cache_hits_total", "type": "counter", "labels": map[string]string{}, "signal": "authn_hits"}, map[string]any{"id": "misses", "metric": "cache_misses_total", "type": "counter", "labels": map[string]string{}, "signal": "authn_misses"}}}}}
	b, _ := json.Marshal(doc)
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	return p
}
func TestMetricsConfigRestrictions(t *testing.T) {
	for _, url := range []string{"http://example.test/metrics", "https://user:private@example.test/metrics", "https://example.test/metrics?token=private", "https://example.test/metrics#private"} {
		if _, err := loadMetricCollector(metricsFixture(t, url)); err == nil {
			t.Fatal("accepted unsafe source")
		}
	}
	p := metricsFixture(t, "http://127.0.0.1:1/metrics")
	c, err := loadMetricCollector(p)
	if err != nil {
		t.Fatal(err)
	}
	c.close()
	b, _ := os.ReadFile(p)
	b = append(b, []byte(" {}")...)
	_ = os.WriteFile(p, b, 0600)
	if _, err := loadMetricCollector(p); err == nil {
		t.Fatal("accepted trailing document")
	}
}

func TestMetricConfigInvalidPermutations(t *testing.T) {
	tests := map[string]func(*metricConfigInput){
		"version": func(c *metricConfigInput) { c.SchemaVersion = "2" },
		"sources": func(c *metricConfigInput) { c.Sources = nil },
		"too-many-sources": func(c *metricConfigInput) {
			for len(c.Sources) < 5 {
				c.Sources = append(c.Sources, c.Sources[0])
			}
		},
		"id":           func(c *metricConfigInput) { c.Sources[0].ID = "private/invalid" },
		"instance":     func(c *metricConfigInput) { c.Sources[0].InstanceID = "" },
		"scope":        func(c *metricConfigInput) { c.Sources[0].ScopeEvidenceRef = "" },
		"isolation":    func(c *metricConfigInput) { c.Sources[0].ExclusiveTraffic = nil },
		"timeout":      func(c *metricConfigInput) { c.Sources[0].Timeout = "31s" },
		"no-selectors": func(c *metricConfigInput) { c.Sources[0].Selectors = nil },
		"duplicate-selector": func(c *metricConfigInput) {
			c.Sources[0].Selectors = append(c.Sources[0].Selectors, c.Sources[0].Selectors[0])
		},
		"metric-name": func(c *metricConfigInput) { c.Sources[0].Selectors[0].Metric = "invalid name" },
		"label-name": func(c *metricConfigInput) {
			c.Sources[0].Selectors[0].Labels = map[string]string{"invalid name": "value"}
		},
		"signal-type":        func(c *metricConfigInput) { c.Sources[0].Selectors[0].Type = "gauge" },
		"duplicate-role":     func(c *metricConfigInput) { c.Sources[0].Selectors[1].Signal = "authn_hits" },
		"missing-credential": func(c *metricConfigInput) { c.Sources[0].BearerTokenFile = "private-missing" },
		"missing-ca":         func(c *metricConfigInput) { c.Sources[0].CAFile = "private-missing" },
	}
	for name, edit := range tests {
		t.Run(name, func(t *testing.T) {
			path := metricsFixture(t, "http://127.0.0.1:1/metrics")
			var cfg metricConfigInput
			b, _ := os.ReadFile(path)
			_ = json.Unmarshal(b, &cfg)
			edit(&cfg)
			b, _ = json.Marshal(cfg)
			_ = os.WriteFile(path, b, 0600)
			if _, err := loadMetricCollector(path); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}
