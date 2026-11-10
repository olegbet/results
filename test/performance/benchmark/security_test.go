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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pb "github.com/tektoncd/results/proto/v1alpha2/results_go_proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/runtime"
	ktest "k8s.io/client-go/testing"
)

type echoErrorFixture struct {
	pb.UnimplementedResultsServer
	detail string
}

func (s echoErrorFixture) ListResults(context.Context, *pb.ListResultsRequest) (*pb.ListResultsResponse, error) {
	return nil, status.Error(codes.Internal, s.detail)
}
func TestBenchmarkSecretProjection(t *testing.T) {
	poolPath := poolFixture(t, 2)
	pool, err := loadTokenPool(poolPath, 0)
	if err != nil {
		t.Fatal(err)
	}
	secret := pool.clients[0].token
	digest := sha256.Sum256([]byte(secret))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("private-invalid-" + secret))
	}))
	defer srv.Close()
	args := apiFixture(t, echoErrorFixture{detail: secret}, nil)
	cases := caseFixture(t, `{"schema_version":"1","cases":[{"id":"probe","operation":"list-results","parent":"ns-00","expected":"allow"}]}`)
	config := metricsFixture(t, srv.URL)
	out := filepath.Join(t.TempDir(), "report.json")
	cmd := newRootCmd()
	cmd.SetArgs(append(append([]string{"query"}, args...), "--token-pool", poolPath, "--access-cases", cases, "--metrics-config", config, "--count", "4", "--transport", "both", "--output", out))
	var commandErr error
	stderr, err := captureStderr(t, func() error {
		stdout, e := captureStdout(t, func() error { return cmd.ExecuteContext(context.Background()) })
		commandErr = e
		checkNoSecrets(t, string(stdout), []string{secret, "Bearer " + secret, hex.EncodeToString(digest[:]), poolPath, config, srv.URL, "private-invalid-"})
		return nil
	})
	if err != nil || commandErr == nil {
		t.Fatal("expected failed diagnostic")
	}
	checkNoSecrets(t, string(stderr)+commandErr.Error(), []string{secret, hex.EncodeToString(digest[:]), poolPath, config, srv.URL})
	saved, _ := os.ReadFile(out)
	checkNoSecrets(t, string(saved), []string{secret, hex.EncodeToString(digest[:]), poolPath, config, srv.URL})
	// Untrusted malformed input and Kubernetes errors must not escape safe reasons.
	bad := caseFixture(t, `{"schema_version":"`+secret+`","sources":[]}`)
	_, err = loadMetricCollector(bad)
	if err == nil {
		t.Fatal("bad config accepted")
	}
	checkNoSecrets(t, err.Error(), []string{secret, bad})
	k, opt := usersFixture(t)
	k.PrependReactor("create", "*", func(ktest.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("Authorization: Bearer %s", secret)
	})
	_, err = prepareUsers(context.Background(), k, "https://fixture", opt)
	if err == nil {
		t.Fatal("bad preparation succeeded")
	}
	checkNoSecrets(t, err.Error(), []string{secret})
	inventory, _ := os.ReadFile(filepath.Join(opt.OutputDir, "ownership.json"))
	checkNoSecrets(t, string(inventory), []string{secret})
}
func checkNoSecrets(t *testing.T, output string, secrets []string) {
	t.Helper()
	for i, secret := range secrets {
		if strings.Contains(output, secret) {
			t.Fatalf("private input %d leaked", i)
		}
	}
}
func captureStderr(t *testing.T, fn func() error) ([]byte, error) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stderr
	os.Stderr = f
	defer func() { os.Stderr = previous; _ = f.Close() }()
	err = fn()
	b, e := os.ReadFile(f.Name())
	if e != nil {
		t.Fatal(e)
	}
	return b, err
}
func TestMetricProjectionExcludesUnselectedSeries(t *testing.T) {
	secret := "synthetic-unselected-label"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprintf(w, "%s\n# TYPE unselected gauge\nunselected{private=%q} 1\n", metricText, secret)
	}))
	defer srv.Close()
	c, err := loadMetricCollector(metricsFixture(t, srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	c.capture(context.Background(), false)
	c.capture(context.Background(), true)
	result, _ := c.results()
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	checkNoSecrets(t, string(b), []string{secret, "unselected", srv.URL})
}

func (s echoErrorFixture) CreateResult(context.Context, *pb.CreateResultRequest) (*pb.Result, error) {
	return nil, status.Error(codes.Internal, s.detail)
}
func TestSendLogAndCleanupErrorsExcludeSecrets(t *testing.T) {
	secret := "synthetic-secret-echo"
	args := apiFixture(t, echoErrorFixture{detail: secret}, nil)
	out := filepath.Join(t.TempDir(), "store.json")
	cmd := newRootCmd()
	cmd.SetArgs(append(append([]string{"store"}, args...), "--token-pool", poolFixture(t, 2), "--count", "2", "--output", out))
	stdout, err := captureStdout(t, func() error { return cmd.ExecuteContext(context.Background()) })
	if err != nil {
		t.Fatal(err)
	}
	sendlog, err := os.ReadFile(strings.TrimSuffix(out, ".json") + ".sendlog.json")
	if err != nil {
		t.Fatal(err)
	}
	checkNoSecrets(t, string(stdout)+string(sendlog), []string{secret, "Authorization:", "Bearer synthetic-user"})
	k, opt := usersFixture(t)
	if _, err := prepareUsers(context.Background(), k, "https://fixture", opt); err != nil {
		t.Fatal(err)
	}
	k.PrependReactor("delete", "*", func(ktest.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("Authorization: Bearer %s", secret)
	})
	if err := cleanupUsers(context.Background(), k, "https://fixture", opt.Context, filepath.Join(opt.OutputDir, "ownership.json")); err == nil {
		t.Fatal("deletion failure hidden")
	} else {
		checkNoSecrets(t, err.Error(), []string{secret})
	}
}
