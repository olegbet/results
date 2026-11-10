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

func TestRunContextDecisionTTLs(t *testing.T) {
	doc := contextDocument()
	path := filepath.Join(t.TempDir(), "context.json")
	write := func() {
		b, _ := json.Marshal(doc)
		if err := os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	cfg := &RunConfig{ServerImage: "fixture", DBBackend: "local"}
	if _, err := loadRunContext(path, cfg); err != nil {
		t.Fatal(err)
	}
	doc["phase"] = "warm"
	write()
	if _, err := loadRunContext(path, cfg); err == nil {
		t.Fatal("warm missing original/prior references accepted")
	}
	doc["phase"] = "cold"
	delete(doc["environment"].(map[string]any), "configuration_ref")
	write()
	if _, err := loadRunContext(path, cfg); err == nil {
		t.Fatal("missing environment receipt accepted")
	}
}

func contextDocument() map[string]any {
	return map[string]any{"schema_version": "1", "scenario_id": "fixture", "phase": "cold", "cache": map[string]any{"enabled": true, "positive_ttl": "60s", "negative_ttl": "10m", "capacity_per_cache": 10000, "provenance": "operator-declared"}, "environment": map[string]any{"server_image": "fixture", "server_revision": "abc", "replicas": 1, "selected_pod": "one", "routing": "pinned", "authentication_enabled": true, "k8s_qps": 500, "k8s_burst": 1000, "db_type": "local", "configuration_ref": "nonexistent-receipt.json", "provenance": "operator-declared"}, "control": map[string]any{"working_set": "one-user", "exclusive_traffic": true, "cold_state_ref": "reset.json", "prior_report_ref": "", "provenance": "operator-declared"}}
}

func TestRunContextStrictControls(t *testing.T) {
	edits := map[string]func(map[string]any){
		"negative-ttl":            func(d map[string]any) { d["cache"].(map[string]any)["negative_ttl"] = "0s" },
		"positive-ttl":            func(d map[string]any) { d["cache"].(map[string]any)["positive_ttl"] = "-1s" },
		"old-ttl":                 func(d map[string]any) { d["cache"].(map[string]any)["authn_ttl"] = "10m" },
		"missing-enabled":         func(d map[string]any) { delete(d["cache"].(map[string]any), "enabled") },
		"missing-isolation":       func(d map[string]any) { delete(d["control"].(map[string]any), "exclusive_traffic") },
		"disabled-auth":           func(d map[string]any) { d["environment"].(map[string]any)["authentication_enabled"] = false },
		"verified-no-evidence":    func(d map[string]any) { d["environment"].(map[string]any)["provenance"] = "verified" },
		"disabled-cache-conflict": func(d map[string]any) { d["phase"] = "disabled" },
		"cli-conflict":            func(d map[string]any) { d["environment"].(map[string]any)["server_image"] = "different" },
	}
	for name, edit := range edits {
		t.Run(name, func(t *testing.T) {
			doc := contextDocument()
			edit(doc)
			b, _ := json.Marshal(doc)
			path := caseFixture(t, string(b))
			if _, err := loadRunContext(path, &RunConfig{ServerImage: "fixture", DBBackend: "local"}); err == nil {
				t.Fatal("invalid controls accepted")
			}
		})
	}
	for _, phase := range []string{"disabled", "cold", "warm", "after-positive-expiry", "after-negative-expiry"} {
		doc := contextDocument()
		doc["phase"] = phase
		if phase == "disabled" {
			doc["cache"].(map[string]any)["enabled"] = false
		}
		doc["control"].(map[string]any)["prior_report_ref"] = "prior.json"
		doc["control"].(map[string]any)["cold_report_ref"] = "original-cold.json"
		b, _ := json.Marshal(doc)
		got, err := loadRunContext(caseFixture(t, string(b)), &RunConfig{ServerImage: "fixture", DBBackend: "local"})
		if err != nil || got.Phase != phase || got.CacheAssessment != "unverified" {
			t.Fatalf("phase %s: %v", phase, err)
		}
	}
}
