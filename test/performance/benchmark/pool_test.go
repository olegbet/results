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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func poolFixture(t *testing.T, n int) string {
	t.Helper()
	dir := t.TempDir()
	clients := make([]map[string]any, n)
	for i := range clients {
		id := fmt.Sprintf("user-%04d", i)
		if err := os.WriteFile(filepath.Join(dir, id), []byte("synthetic-"+id), 0600); err != nil {
			t.Fatal(err)
		}
		clients[i] = map[string]any{"id": id, "token_file": id, "identity": map[string]any{"username": id, "provenance": "verified", "evidence_ref": "receipt.json"}}
	}
	b, err := json.Marshal(map[string]any{"schema_version": "1", "population_id": "fixture", "clients": clients})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "pool.json")
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func TestPoolStrictAndImmutable(t *testing.T) {
	path := poolFixture(t, 2)
	p, err := loadTokenPool(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.summary.SupportedDistinctUsers != 2 {
		t.Fatal("lost supported identities")
	}
	fingerprint := p.summary.PoolFingerprint
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "user-0000"), []byte("synthetic-changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if p.clients[0].token != "synthetic-user-0000" {
		t.Fatal("runtime token changed")
	}
	changed, err := loadTokenPool(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if changed.summary.PoolFingerprint == fingerprint {
		t.Fatal("fingerprint missed changed token")
	}
	for _, bad := range []string{"", " \n ", "two tokens", "synthetic-user-0001"} {
		if err := os.WriteFile(filepath.Join(filepath.Dir(path), "user-0000"), []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadTokenPool(path, 0); err == nil {
			t.Fatal("accepted malformed/duplicate token")
		} else if bad != "" && strings.Contains(err.Error(), bad) {
			t.Fatal("error echoed token")
		}
	}
}
func TestPoolInputValidation(t *testing.T) {
	for _, change := range []string{"unknown", "trailing", "duplicate-label", "expired", "short-expiry", "missing-evidence", "empty", "bad-version", "bad-identity"} {
		t.Run(change, func(t *testing.T) {
			path := poolFixture(t, 2)
			b, _ := os.ReadFile(path)
			var doc map[string]any
			if err := json.Unmarshal(b, &doc); err != nil {
				t.Fatal(err)
			}
			clients := doc["clients"].([]any)
			first := clients[0].(map[string]any)
			switch change {
			case "unknown":
				doc["secret-untrusted-field"] = "synthetic-secret"
			case "duplicate-label":
				first["id"] = "user-0001"
			case "expired":
				first["expires_at"] = time.Now().Add(-time.Hour).Format(time.RFC3339)
			case "short-expiry":
				first["expires_at"] = time.Now().Add(time.Second).Format(time.RFC3339)
			case "missing-evidence":
				delete(first["identity"].(map[string]any), "evidence_ref")
			case "empty":
				doc["clients"] = []any{}
			case "bad-version":
				doc["schema_version"] = "2"
			case "bad-identity":
				first["identity"] = nil
			}
			b, _ = json.Marshal(doc)
			if change == "trailing" {
				b = append(b, []byte(" {}")...)
			}
			if err := os.WriteFile(path, b, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadTokenPool(path, time.Minute); err == nil {
				t.Fatal("accepted invalid configuration")
			} else if strings.Contains(err.Error(), "synthetic-secret") {
				t.Fatal("unsafe error")
			}
		})
	}
}
func TestPoolProvenanceDeduplicatesUsers(t *testing.T) {
	path := poolFixture(t, 3)
	b, _ := os.ReadFile(path)
	var doc map[string]any
	_ = json.Unmarshal(b, &doc)
	clients := doc["clients"].([]any)
	for _, c := range clients {
		c.(map[string]any)["identity"].(map[string]any)["username"] = "one-user"
	}
	clients[0].(map[string]any)["identity"].(map[string]any)["provenance"] = "declared"
	b, _ = json.Marshal(doc)
	_ = os.WriteFile(path, b, 0600)
	p, err := loadTokenPool(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.summary.DistinctUsers != 1 || p.summary.SupportedDistinctUsers != 0 || p.summary.DeclaredUsers != 1 {
		t.Fatal("inflated identity provenance")
	}
}

func TestPoolConflictingUIDAndPrivatePathFingerprint(t *testing.T) {
	path := poolFixture(t, 2)
	b, _ := os.ReadFile(path)
	var doc map[string]any
	_ = json.Unmarshal(b, &doc)
	clients := doc["clients"].([]any)
	for _, c := range clients {
		c.(map[string]any)["identity"].(map[string]any)["uid"] = "same-uid"
	}
	b, _ = json.Marshal(doc)
	_ = os.WriteFile(path, b, 0600)
	p, err := loadTokenPool(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.summary.SupportedDistinctUsers != 0 || p.summary.DeclaredUsers != 2 {
		t.Fatal("conflicting identity mappings certified")
	}
	for _, c := range clients {
		entry := c.(map[string]any)
		entry["token_file"] = filepath.Join(filepath.Dir(path), entry["token_file"].(string))
		entry["identity"].(map[string]any)["evidence_ref"] = "other-receipt.json"
	}
	b, _ = json.Marshal(doc)
	_ = os.WriteFile(path, b, 0600)
	changed, err := loadTokenPool(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.summary.PoolFingerprint != changed.summary.PoolFingerprint {
		t.Fatal("private path/evidence changed fingerprint")
	}
}
