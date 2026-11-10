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
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/tektoncd/results/proto/v1alpha2/results_go_proto"
	"github.com/tektoncd/results/test/performance/metrics"
	"google.golang.org/grpc"
)

func caseFixture(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cases.json")
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}
func TestCaseMatrixAndTraversal(t *testing.T) {
	path := caseFixture(t, `{"schema_version":"1","cases":[{"id":"list","operation":"list-results","parent":"ns-00","expected":"allow"},{"id":"get","operation":"get-result","name":"ns-00/results/test","expected":"deny"}]}`)
	cases, err := loadAccessCases(path)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for i := 0; i < 12; i++ {
		client, entry, transport := caseAssignment(i, 3, len(cases.Cases), "both")
		seen[fmt.Sprintf("%d-%d-%s", client, entry, transport)] = true
	}
	if len(seen) != 12 {
		t.Fatal("incomplete Cartesian coverage")
	}
	m := metrics.NewMetricSet()
	m.Start()
	out := executeAccessCase(context.Background(), grpcReader{client: repeatingResults{}}, cases.Cases[0], "allow", 100, m)
	m.Stop()
	if out != "unexpected-error" {
		t.Fatal("repeated pagination token passed")
	}
}

type repeatingResults struct{ pb.ResultsClient }

func (repeatingResults) ListResults(context.Context, *pb.ListResultsRequest, ...grpc.CallOption) (*pb.ListResultsResponse, error) {
	return &pb.ListResultsResponse{NextPageToken: "same"}, nil
}

func TestAccessCaseStrictTargets(t *testing.T) {
	for _, body := range []string{
		`{"schema_version":"1","cases":[{"id":"x","operation":"get-result","name":"ns-/results/test","expected":"allow"}]}`,
		`{"schema_version":"1","cases":[{"id":"x","operation":"get-record","name":"ns/results/test","expected":"allow"}]}`,
		`{"schema_version":"1","cases":[{"id":"x","operation":"list-results","parent":"ns","name":"extra","expected":"allow"}]}`,
		`{"schema_version":"1","cases":[{"id":"x","operation":"get-result","name":"ns/results/test","filter":"extra","expected":"allow"}]}`,
		`{"schema_version":"1","cases":[{"id":"x","operation":"get-result","name":"ns/results/test","expected":"other"}]}`,
		`{"schema_version":"1","cases":[{"id":"x","operation":"list-results","parent":"ns","expected":"allow"},{"id":"x","operation":"list-results","parent":"ns","expected":"deny"}]}`,
	} {
		if _, err := loadAccessCases(caseFixture(t, body)); err == nil {
			t.Fatal("invalid case accepted")
		}
	}
}

type errorReader struct {
	accessReader
	err error
}

func (r errorReader) GetResult(context.Context, *pb.GetResultRequest) (*pb.Result, error) {
	return nil, r.err
}
func TestAccessCaseTimeoutAndCancellation(t *testing.T) {
	c := accessCase{Operation: "get-result", Name: "ns/results/test"}
	for _, tc := range []struct {
		code           codes.Code
		cancel         bool
		expected, want string
	}{
		{codes.PermissionDenied, false, "deny", "expected-rejection"},
		{codes.Unauthenticated, false, "invalid", "expected-rejection"},
		{codes.PermissionDenied, false, "allow", "unexpected-rejection"},
		{codes.NotFound, false, "deny", "unexpected-error"},
		{codes.Internal, false, "deny", "unexpected-error"},
		{codes.DeadlineExceeded, false, "allow", "unexpected-error"},
		{codes.Canceled, true, "allow", "incomplete"},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		if tc.cancel {
			cancel()
		}
		m := metrics.NewMetricSet()
		m.Start()
		got := executeAccessCase(ctx, errorReader{err: status.Error(tc.code, "private")}, c, tc.expected, 1, m)
		m.Stop()
		cancel()
		if got != tc.want {
			t.Fatalf("%v: %s", tc.code, got)
		}
	}
}
