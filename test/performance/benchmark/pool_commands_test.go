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
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	pb "github.com/tektoncd/results/proto/v1alpha2/results_go_proto"
	"github.com/tektoncd/results/test/performance/report"
)

type writableFixture struct{ queryResultsServer }

func (writableFixture) CreateResult(_ context.Context, r *pb.CreateResultRequest) (*pb.Result, error) {
	return r.Result, nil
}
func (writableFixture) CreateRecord(_ context.Context, r *pb.CreateRecordRequest) (*pb.Record, error) {
	return r.Record, nil
}
func (writableFixture) UpdateResult(_ context.Context, r *pb.UpdateResultRequest) (*pb.Result, error) {
	return r.Result, nil
}
func (writableFixture) UpdateRecord(_ context.Context, r *pb.UpdateRecordRequest) (*pb.Record, error) {
	return r.Record, nil
}
func TestPoolCommandModes(t *testing.T) {
	args := apiFixture(t, writableFixture{}, nil)
	pool := poolFixture(t, 8)
	for _, mode := range []string{"load", "store", "query", "mixed"} {
		t.Run(mode, func(t *testing.T) {
			cmd := newRootCmd()
			flags := append([]string{mode}, args...)
			flags = append(flags, "--token-pool", pool, "--count", "16", "--namespaces", "1", "--concurrency", "8", "--child-min", "2", "--child-max", "2", "--output", filepath.Join(t.TempDir(), "report.json"))
			cmd.SetArgs(flags)
			stdout, err := captureStdout(t, func() error { return cmd.ExecuteContext(context.Background()) })
			if err != nil {
				t.Fatal(err)
			}
			var got report.Report
			if err := json.Unmarshal(stdout, &got); err != nil {
				t.Fatal(err)
			}
			if got.Config.ClientPool == nil || got.Config.ClientPool.ConfiguredClients != 8 {
				t.Fatal("missing pool metadata")
			}
			var ops int64
			for _, c := range got.Metrics.Clients {
				ops += c.TotalOps
			}
			if ops != got.Metrics.TotalOps {
				t.Fatal("command totals do not reconcile")
			}
		})
	}
}

func TestPoolVerificationUsesFirstClientAndKeepsArtifacts(t *testing.T) {
	var mu sync.Mutex
	var verificationTokens []string
	args := apiFixture(t, writableFixture{}, func(r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/ListRecords") {
			mu.Lock()
			verificationTokens = append(verificationTokens, r.Header.Get("Authorization"))
			mu.Unlock()
		}
	})
	pool := poolFixture(t, 4)
	out := filepath.Join(t.TempDir(), "load.json")
	cmd := newRootCmd()
	cmd.SetArgs(append(append([]string{"load"}, args...), "--token-pool", pool, "--count", "2", "--namespaces", "1", "--verify", "--output", out))
	stdout, err := captureStdout(t, func() error { return cmd.ExecuteContext(context.Background()) })
	if err == nil || !strings.Contains(err.Error(), "seed verification failed") {
		t.Fatal("expected verification failure")
	}
	if !json.Valid(stdout) {
		t.Fatal("verification failure lost report")
	}
	for _, token := range verificationTokens {
		if token != "Bearer synthetic-user-0000" {
			t.Fatal("verification switched credential")
		}
	}
	if len(verificationTokens) == 0 {
		t.Fatal("verification not attempted")
	}
	if _, err := os.Stat(strings.TrimSuffix(out, ".json") + ".sendlog.json"); err != nil {
		t.Fatal("verification failure lost send log")
	}
}

func TestPoolFlagsConflictBeforeRequests(t *testing.T) {
	pool := poolFixture(t, 1)
	for _, args := range [][]string{{"query", "--token-pool", pool, "--token", "private"}, {"query", "--token-pool", pool, "--concurrency", "0"}, {"dataset", "--token-pool", pool}} {
		cmd := newRootCmd()
		cmd.SetArgs(args)
		if err := cmd.ExecuteContext(context.Background()); err == nil {
			t.Fatal("accepted invalid flags")
		}
	}
	t.Setenv("SA_TOKEN_FILE", "private-nonexistent-fallback")
	args := apiFixture(t, queryResultsServer{}, nil)
	cmd := newRootCmd()
	cmd.SetArgs(append(append([]string{"query"}, args...), "--token-pool", pool, "--count", "1", "--output", filepath.Join(t.TempDir(), "report.json")))
	if _, err := captureStdout(t, func() error { return cmd.ExecuteContext(context.Background()) }); err != nil {
		t.Fatal("pool inherited single-token fallback")
	}
}
