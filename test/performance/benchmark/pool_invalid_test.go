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
	"testing"
)

func TestInvalidPoolExpectationCrossValidation(t *testing.T) {
	p := &tokenPool{clients: []poolClient{{poolEntry: poolEntry{ID: "valid", Identity: &poolIdentity{Username: "user", Provenance: "declared"}}}, {poolEntry: poolEntry{ID: "invalid", IntentionallyInvalid: true}}}}
	cases := &accessCases{Cases: []accessCase{{Expected: "allow", ExpectedByClient: map[string]string{"invalid": "invalid"}}}}
	if err := validateCaseClients(p, cases); err != nil {
		t.Fatal(err)
	}
	p.countUsers()
	if p.summary.DistinctUsers != 1 {
		t.Fatal("invalid credential inflated users")
	}
	cases.Cases = append(cases.Cases, accessCase{Expected: "deny"})
	if validateCaseClients(p, cases) == nil {
		t.Fatal("invalid credential resolved to deny")
	}
	cases.Cases = cases.Cases[:1]
	cases.Cases[0].ExpectedByClient["unknown"] = "allow"
	if validateCaseClients(p, cases) == nil {
		t.Fatal("unknown override accepted")
	}
	if validateCaseClients(nil, &accessCases{Cases: []accessCase{{Expected: "deny", ExpectedByClient: map[string]string{"single": "allow"}}}}) != nil {
		t.Fatal("single override rejected")
	}
}
