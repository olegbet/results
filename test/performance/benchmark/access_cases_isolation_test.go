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
	"sync/atomic"
	"testing"

	pb "github.com/tektoncd/results/proto/v1alpha2/results_go_proto"
	"github.com/tektoncd/results/test/performance/report"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type permissionFixture struct {
	pb.UnimplementedResultsServer
	wrong bool
}

func (p permissionFixture) decision(ctx context.Context) error {
	md, _ := metadata.FromIncomingContext(ctx)
	token := md.Get("authorization")
	allowed := len(token) == 1 && token[0] == "Bearer synthetic-user-0000"
	if p.wrong {
		allowed = !allowed
	}
	if !allowed {
		return status.Error(codes.PermissionDenied, "private-server-detail")
	}
	return nil
}
func (p permissionFixture) GetResult(ctx context.Context, _ *pb.GetResultRequest) (*pb.Result, error) {
	return &pb.Result{}, p.decision(ctx)
}
func (p permissionFixture) GetRecord(ctx context.Context, _ *pb.GetRecordRequest) (*pb.Record, error) {
	return &pb.Record{}, p.decision(ctx)
}
func (p permissionFixture) ListResults(ctx context.Context, _ *pb.ListResultsRequest) (*pb.ListResultsResponse, error) {
	return &pb.ListResultsResponse{}, p.decision(ctx)
}
func (p permissionFixture) ListRecords(ctx context.Context, _ *pb.ListRecordsRequest) (*pb.ListRecordsResponse, error) {
	return &pb.ListRecordsResponse{}, p.decision(ctx)
}

func TestPermissionMatrixCommand(t *testing.T) {
	cases := caseFixture(t, `{"schema_version":"1","cases":[
 {"id":"lr","operation":"list-results","parent":"ns-00","expected":"allow","expected_by_client":{"user-0001":"deny"}},
 {"id":"lc","operation":"list-records","parent":"ns-00/results/test","expected":"allow","expected_by_client":{"user-0001":"deny"}},
 {"id":"gr","operation":"get-result","name":"ns-00/results/test","expected":"allow","expected_by_client":{"user-0001":"deny"}},
 {"id":"gc","operation":"get-record","name":"ns-00/results/test/records/test","expected":"allow","expected_by_client":{"user-0001":"deny"}}]}`)
	for _, wrong := range []bool{false, true} {
		args := apiFixture(t, permissionFixture{wrong: wrong}, nil)
		out := filepath.Join(t.TempDir(), "report.json")
		cmd := newRootCmd()
		cmd.SetArgs(append(append([]string{"query"}, args...), "--token-pool", poolFixture(t, 2), "--access-cases", cases, "--transport", "both", "--count", "32", "--output", out))
		stdout, err := captureStdout(t, func() error { return cmd.ExecuteContext(context.Background()) })
		if (err != nil) != wrong {
			t.Fatalf("wrong=%v error=%v", wrong, err)
		}
		saved, _ := os.ReadFile(out)
		if string(saved) != string(stdout) {
			t.Fatal("diagnostic report mismatch")
		}
		var r report.Report
		if err := json.Unmarshal(stdout, &r); err != nil {
			t.Fatal(err)
		}
		expected := "passed"
		if wrong {
			expected = "failed"
		}
		if r.Validation.Responses.Status != expected || len(r.Validation.CaseResults) != 16 {
			t.Fatal("matrix result incorrect")
		}
		if !wrong && r.Metrics.Clients[1].ExpectedRejections != 16 {
			t.Fatal("expected rejections lost")
		}
		if wrong && (r.Metrics.Clients[0].UnexpectedRejections != 16 || r.Metrics.Clients[1].UnexpectedGrants != 16) {
			t.Fatal("wrong decisions missed")
		}
	}
}

func TestCaseMismatchPrecedesLaterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int64
	args := apiFixture(t, permissionFixture{}, func(*http.Request) {
		if calls.Add(1) > 1 {
			cancel()
		}
	})
	cases := caseFixture(t, `{"schema_version":"1","cases":[{"id":"list","operation":"list-results","parent":"ns-00","expected":"deny"}]}`)
	cmd := newRootCmd()
	cmd.SetArgs(append(append([]string{"query"}, args...), "--token-pool", poolFixture(t, 2), "--access-cases", cases, "--count", "100", "--concurrency", "1", "--output", filepath.Join(t.TempDir(), "report.json")))
	stdout, err := captureStdout(t, func() error { return cmd.ExecuteContext(ctx) })
	if err == nil {
		t.Fatal("mismatch/cancellation succeeded")
	}
	var r report.Report
	if err := json.Unmarshal(stdout, &r); err != nil {
		t.Fatal(err)
	}
	if r.Validation.Responses.Status != "failed" || r.Validation.Execution.Status != "interrupted" || r.Metrics.Clients[0].UnexpectedGrants < 1 {
		t.Fatal("cancellation erased mismatch")
	}
}

// The same authenticated user may list Results in ns-00 but cannot get them,
// list records, or list Results in ns-01. Each access attribute stays independent.
type scopedPermissionFixture struct{ permissionFixture }

func (s scopedPermissionFixture) ListResults(ctx context.Context, r *pb.ListResultsRequest) (*pb.ListResultsResponse, error) {
	if r.Parent != "ns-00" {
		return nil, status.Error(codes.PermissionDenied, "scope")
	}
	return s.permissionFixture.ListResults(ctx, r)
}
func (scopedPermissionFixture) ListRecords(context.Context, *pb.ListRecordsRequest) (*pb.ListRecordsResponse, error) {
	return nil, status.Error(codes.PermissionDenied, "resource")
}
func (scopedPermissionFixture) GetResult(context.Context, *pb.GetResultRequest) (*pb.Result, error) {
	return nil, status.Error(codes.PermissionDenied, "verb")
}
func (scopedPermissionFixture) GetRecord(context.Context, *pb.GetRecordRequest) (*pb.Record, error) {
	return nil, status.Error(codes.PermissionDenied, "resource and verb")
}
func TestPermissionNamespaceResourceVerbIsolation(t *testing.T) {
	cases := caseFixture(t, `{"schema_version":"1","cases":[
 {"id":"allowed","operation":"list-results","parent":"ns-00","expected":"deny","expected_by_client":{"user-0000":"allow"}},
 {"id":"namespace","operation":"list-results","parent":"ns-01","expected":"deny"},
 {"id":"resource","operation":"list-records","parent":"ns-00/results/test","expected":"deny"},
 {"id":"verb","operation":"get-result","name":"ns-00/results/test","expected":"deny"},
 {"id":"record","operation":"get-record","name":"ns-00/results/test/records/test","expected":"deny"}]}`)
	args := apiFixture(t, scopedPermissionFixture{}, nil)
	cmd := newRootCmd()
	cmd.SetArgs(append(append([]string{"query"}, args...), "--token-pool", poolFixture(t, 2), "--access-cases", cases, "--transport", "both", "--count", "20", "--output", filepath.Join(t.TempDir(), "report.json")))
	stdout, err := captureStdout(t, func() error { return cmd.ExecuteContext(context.Background()) })
	if err != nil {
		t.Fatal(err)
	}
	var r report.Report
	_ = json.Unmarshal(stdout, &r)
	if r.Validation.Responses.Status != "passed" || r.Validation.Coverage.CompletedPairs != 20 || r.Metrics.Clients[0].ExpectedRejections != 8 || r.Metrics.Clients[1].ExpectedRejections != 10 {
		t.Fatal("permission attributes not isolated")
	}
}
