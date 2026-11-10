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
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	pb "github.com/tektoncd/results/proto/v1alpha2/results_go_proto"
	"google.golang.org/grpc"
)

// apiFixture serves both real TLS transports without a cluster. Request hooks
// may inspect synthetic credentials or block until request-context cancellation.
func apiFixture(t *testing.T, api pb.ResultsServer, hook func(*http.Request)) []string {
	t.Helper()
	gc := grpc.NewServer()
	pb.RegisterResultsServer(gc, api)
	t.Cleanup(gc.Stop)
	mux := runtime.NewServeMux()
	if err := pb.RegisterResultsHandlerServer(context.Background(), mux, api); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hook != nil {
			hook(r)
		}
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
			gc.ServeHTTP(w, r)
		} else {
			mux.ServeHTTP(w, r)
		}
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	cert := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	return []string{"--server-addr", srv.URL, "--server-name", "127.0.0.1", "--cert", cert}
}

func TestAPIFixtureTransports(t *testing.T) {
	args := apiFixture(t, queryResultsServer{}, nil)
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("fixture-token"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := newRootCmd()
	cmd.SetArgs(append([]string{"query"}, append(args, "--token", token, "--transport", "both", "--count", "2", "--output", filepath.Join(t.TempDir(), "report.json"))...))
	if _, err := captureStdout(t, func() error { return cmd.ExecuteContext(context.Background()) }); err != nil {
		t.Fatal(err)
	}
}
