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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	pb "github.com/tektoncd/results/proto/v1alpha2/results_go_proto"
	"github.com/tektoncd/results/test/e2e/client"
	"github.com/tektoncd/results/test/performance/metrics"
	"github.com/tektoncd/results/test/performance/report"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type accessCase struct {
	ID               string            `json:"id"`
	Operation        string            `json:"operation"`
	Parent           string            `json:"parent,omitempty"`
	Name             string            `json:"name,omitempty"`
	Filter           string            `json:"filter,omitempty"`
	Expected         string            `json:"expected"`
	ExpectedByClient map[string]string `json:"expected_by_client,omitempty"`
}
type accessCases struct {
	SchemaVersion string       `json:"schema_version"`
	Cases         []accessCase `json:"cases"`
	fingerprint   string
}

var namespaceName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
var resultName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*/results/[^/[:space:]]+$`)
var recordName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*/results/[^/[:space:]]+/records/[^/[:space:]]+$`)

func validExpectation(s string) bool { return s == "allow" || s == "deny" || s == "invalid" }
func loadAccessCases(path string) (*accessCases, error) {
	var in accessCases
	if err := readStrictJSON(path, &in); err != nil {
		return nil, fmt.Errorf("access cases: %w", err)
	}
	if in.SchemaVersion != "1" || len(in.Cases) == 0 {
		return nil, fmt.Errorf("access cases: invalid-header")
	}
	ids := map[string]bool{}
	for i, c := range in.Cases {
		if !publicID.MatchString(c.ID) || ids[c.ID] || !validExpectation(c.Expected) {
			return nil, fmt.Errorf("access case %d: invalid-id-or-expectation", i)
		}
		ids[c.ID] = true
		valid := false
		switch c.Operation {
		case "list-results":
			valid = namespaceName.MatchString(c.Parent) && c.Name == ""
		case "list-records":
			valid = resultName.MatchString(c.Parent) && namespaceName.MatchString(strings.Split(c.Parent, "/")[0]) && c.Name == ""
		case "get-result":
			valid = resultName.MatchString(c.Name) && namespaceName.MatchString(strings.Split(c.Name, "/")[0]) && c.Parent == "" && c.Filter == ""
		case "get-record":
			valid = recordName.MatchString(c.Name) && namespaceName.MatchString(strings.Split(c.Name, "/")[0]) && c.Parent == "" && c.Filter == ""
		}
		if !valid {
			return nil, fmt.Errorf("access case %d: invalid-operation-target", i)
		}
		for id, expected := range c.ExpectedByClient {
			if !publicID.MatchString(id) || !validExpectation(expected) {
				return nil, fmt.Errorf("access case %d: invalid-override", i)
			}
		}
	}
	b, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("access cases: fingerprint-failed")
	}
	digest := sha256.Sum256(b)
	in.fingerprint = hex.EncodeToString(digest[:])
	return &in, nil
}
func (c accessCase) expectation(id string) string {
	if override, ok := c.ExpectedByClient[id]; ok {
		return override
	}
	return c.Expected
}
func caseAssignment(index, clients, cases int, transport string) (int, int, string) {
	if transport == "both" {
		transport = "grpc"
		if (index/(clients*cases))%2 != 0 {
			transport = "rest"
		}
	}
	return index % clients, (index / clients) % cases, transport
}

type accessReader interface {
	lister
	GetResult(context.Context, *pb.GetResultRequest) (*pb.Result, error)
	GetRecord(context.Context, *pb.GetRecordRequest) (*pb.Record, error)
}
type grpcReader struct{ client pb.ResultsClient }

func (g grpcReader) ListResults(ctx context.Context, r *pb.ListResultsRequest) (*pb.ListResultsResponse, error) {
	return g.client.ListResults(ctx, r)
}
func (g grpcReader) ListRecords(ctx context.Context, r *pb.ListRecordsRequest) (*pb.ListRecordsResponse, error) {
	return g.client.ListRecords(ctx, r)
}
func (g grpcReader) GetResult(ctx context.Context, r *pb.GetResultRequest) (*pb.Result, error) {
	return g.client.GetResult(ctx, r)
}
func (g grpcReader) GetRecord(ctx context.Context, r *pb.GetRecordRequest) (*pb.Record, error) {
	return g.client.GetRecord(ctx, r)
}
func authRejection(err error) bool {
	if status.Code(err) == codes.Unauthenticated || status.Code(err) == codes.PermissionDenied {
		return true
	}
	var httpErr *client.HTTPStatusError
	return errors.As(err, &httpErr) && (httpErr.StatusCode == 401 || httpErr.StatusCode == 403)
}
func executeAccessCase(ctx context.Context, reader accessReader, c accessCase, expected string, pageSize int32, m *metrics.MetricSet) string {
	token := ""
	seen := map[string]bool{}
	for page := 0; page < 100000; page++ {
		next := ""
		err := observe(ctx, m, c.Operation, func() error {
			switch c.Operation {
			case "get-result":
				_, err := reader.GetResult(ctx, &pb.GetResultRequest{Name: c.Name})
				return err
			case "get-record":
				_, err := reader.GetRecord(ctx, &pb.GetRecordRequest{Name: c.Name})
				return err
			case "list-results":
				resp, err := reader.ListResults(ctx, &pb.ListResultsRequest{Parent: c.Parent, Filter: c.Filter, PageToken: token, PageSize: pageSize})
				next = resp.GetNextPageToken()
				return err
			case "list-records":
				resp, err := reader.ListRecords(ctx, &pb.ListRecordsRequest{Parent: c.Parent, Filter: c.Filter, PageToken: token, PageSize: pageSize})
				next = resp.GetNextPageToken()
				return err
			default:
				return fmt.Errorf("unsupported access operation")
			}
		})
		if err != nil {
			if ctx.Err() != nil {
				return "incomplete"
			}
			if authRejection(err) {
				if expected != "allow" {
					return "expected-rejection"
				}
				return "unexpected-rejection"
			}
			return "unexpected-error"
		}
		if expected != "allow" {
			return "unexpected-grant"
		}
		if next == "" {
			return "allowed"
		}
		if seen[next] {
			localObserve(ctx, m, c.Operation, fmt.Errorf("pagination repeated"))
			return "unexpected-error"
		}
		seen[next] = true
		token = next
	}
	localObserve(ctx, m, c.Operation, fmt.Errorf("pagination limit"))
	return "unexpected-error"
}
func (s *runState) executeCase(ctx context.Context, index int, transport string, clients *APIClients, pageSize int32, m *metrics.MetricSet) {
	clientIndex, caseIndex, transport := caseAssignment(index, len(s.counters), len(s.cases.Cases), transport)
	c := s.cases.Cases[caseIndex]
	counter := s.counters[clientIndex]
	expected := c.expectation(counter.value.ID)
	var reader accessReader = grpcReader{client: clients.GRPC}
	if transport == "rest" {
		reader = clients.REST
	}
	outcome := executeAccessCase(ctx, reader, c, expected, pageSize, m)
	key := fmt.Sprintf("%d/%d/%s", clientIndex, caseIndex, transport)
	s.mu.Lock()
	entry, ok := s.caseResults[key]
	if !ok {
		entry = report.CaseResult{ClientID: counter.value.ID, CaseID: c.ID, Transport: transport, Expected: expected}
	}
	entry.LogicalStarted++
	if outcome != "incomplete" {
		entry.LogicalCompleted++
	}
	mismatch := outcome == "unexpected-grant" || outcome == "unexpected-rejection" || outcome == "unexpected-error"
	if mismatch {
		entry.Mismatches++
	}
	s.caseResults[key] = entry
	s.mu.Unlock()
	counter.mu.Lock()
	switch outcome {
	case "expected-rejection":
		counter.value.ExpectedRejections++
	case "unexpected-grant":
		counter.value.UnexpectedGrants++
	case "unexpected-rejection":
		counter.value.UnexpectedRejections++
	case "unexpected-error":
		counter.value.UnexpectedErrors++
	}
	counter.mu.Unlock()
}
