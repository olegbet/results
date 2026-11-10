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
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/tektoncd/results/test/performance/report"
)

func TestMetricOnlyLifecycleAndUnavailableSource(t *testing.T) {
	args := apiFixture(t, queryResultsServer{}, nil)
	token := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(token, []byte("synthetic-workload"), 0600)
	for _, unavailable := range []bool{false, true} {
		var attempts int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts++
			if r.Header.Get("Authorization") != "" {
				t.Error("inherited workload token")
			}
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(metricText))
		}))
		url := srv.URL
		if unavailable {
			srv.Close()
		}
		cmd := newRootCmd()
		cmd.SetArgs(append(append([]string{"query"}, args...), "--token", token, "--metrics-config", metricsFixture(t, url), "--count", "2", "--output", filepath.Join(t.TempDir(), "report.json")))
		stdout, err := captureStdout(t, func() error { return cmd.ExecuteContext(context.Background()) })
		if err != nil {
			t.Fatal(err)
		}
		var got report.Report
		if err := json.Unmarshal(stdout, &got); err != nil {
			t.Fatal(err)
		}
		if got.Config.ClientPool != nil || len(got.Metrics.Clients) != 0 || got.Metrics.TotalOps != 4 {
			t.Fatal("collector changed workload accounting")
		}
		if unavailable {
			if got.Validation.MetricsCapture.Status != "unavailable" {
				t.Fatal("unavailable source lost")
			}
		} else {
			if attempts != 2 || got.Validation.MetricsCapture.Status != "complete" {
				t.Fatal("missing automatic capture")
			}
			if got.Metrics.Capture.Sources[0].Before.EndedAt > got.Validation.MeasurementWindow.StartedAt {
				t.Fatal("before collection inside measurement")
			}
		}
		srv.Close()
	}
}

func TestMetricFinalCaptureAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	args := apiFixture(t, queryResultsServer{}, func(*http.Request) { cancel() })
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(metricText))
	}))
	defer srv.Close()
	cmd := newRootCmd()
	cmd.SetArgs(append(append([]string{"query"}, args...), "--token-pool", poolFixture(t, 2), "--metrics-config", metricsFixture(t, srv.URL), "--count", "100", "--output", filepath.Join(t.TempDir(), "report.json")))
	stdout, err := captureStdout(t, func() error { return cmd.ExecuteContext(ctx) })
	if err == nil {
		t.Fatal("canceled extended run succeeded")
	}
	var got report.Report
	if err := json.Unmarshal(stdout, &got); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || got.Metrics.Capture.Sources[0].After.Status != "available" || got.Validation.Execution.Status != "interrupted" {
		t.Fatal("final capture canceled with workload or relabeled execution")
	}
}

func TestMetricCaptureAllModes(t *testing.T) {
	args := apiFixture(t, writableFixture{}, nil)
	for _, mode := range []string{"load", "store", "query", "mixed"} {
		t.Run(mode, func(t *testing.T) {
			attempts := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				attempts++
				w.Header().Set("Content-Type", "text/plain")
				_, _ = w.Write([]byte(metricText))
			}))
			defer srv.Close()
			out := filepath.Join(t.TempDir(), "report.json")
			cmd := newRootCmd()
			cmd.SetArgs(append(append([]string{mode}, args...), "--token-pool", poolFixture(t, 2), "--metrics-config", metricsFixture(t, srv.URL), "--count", "4", "--concurrency", "2", "--output", out))
			stdout, err := captureStdout(t, func() error { return cmd.ExecuteContext(context.Background()) })
			if err != nil {
				t.Fatal(err)
			}
			saved, _ := os.ReadFile(out)
			if string(saved) != string(stdout) {
				t.Fatal("capture report outputs differ")
			}
			var got report.Report
			_ = json.Unmarshal(stdout, &got)
			source := got.Metrics.Capture.Sources[0]
			window := got.Validation.MeasurementWindow
			if attempts != 2 || source.Before.EndedAt > window.StartedAt || source.After.StartedAt < window.EndedAt {
				t.Fatal("capture lifecycle overlaps workload")
			}
		})
	}
	cmd := newRootCmd()
	cmd.SetArgs([]string{"dataset", "--metrics-config", "private"})
	if cmd.Execute() == nil {
		t.Fatal("dataset accepted collector")
	}
}
