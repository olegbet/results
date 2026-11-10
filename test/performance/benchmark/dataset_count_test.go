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
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/tektoncd/results/test/performance/report"
)

func TestExplicitDatasetCountSeparatesQueryWork(t *testing.T) {
	args := apiFixture(t, queryResultsServer{}, nil)
	var hash string
	for _, count := range []string{"2", "4"} {
		cmd := newRootCmd()
		cmd.SetArgs(append(append([]string{"query"}, args...), "--token-pool", poolFixture(t, 2), "--dataset-count", "3", "--count", count, "--namespaces", "1", "--output", filepath.Join(t.TempDir(), "report.json")))
		stdout, err := captureStdout(t, func() error { return cmd.ExecuteContext(context.Background()) })
		if err != nil {
			t.Fatal(err)
		}
		var got report.Report
		_ = json.Unmarshal(stdout, &got)
		if hash != "" && hash != got.Meta.DatasetHash {
			t.Fatal("repetitions changed seed hash")
		}
		hash = got.Meta.DatasetHash
	}
}

func TestDatasetCountRestrictions(t *testing.T) {
	for _, args := range [][]string{{"query", "--dataset-count", "0"}, {"query", "--dataset-count", "-1"}, {"load", "--dataset-count", "1"}, {"store", "--dataset-count", "1"}, {"mixed", "--dataset-count", "1"}, {"dataset", "--dataset-count", "1"}} {
		cmd := newRootCmd()
		cmd.SetArgs(args)
		if cmd.Execute() == nil {
			t.Fatal("invalid dataset-count accepted")
		}
	}
}
