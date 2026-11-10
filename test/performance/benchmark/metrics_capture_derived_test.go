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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestMetricCounterDeltasAndRatios(t *testing.T) {
	for _, tc := range []struct {
		name, after string
		wantStatus  string
		wantRatio   float64
	}{{"delta", strings.ReplaceAll(strings.ReplaceAll(metricText, "total 7", "total 11"), "total 3", "total 5"), "available", 4.0 / 6.0}, {"zero", metricText, "unavailable", 0}, {"reset", strings.ReplaceAll(metricText, "total 7", "total 1"), "invalid", 0}, {"restart", strings.ReplaceAll(metricText, "seconds 123", "seconds 456"), "invalid", 0}} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				body := metricText
				if requests.Add(1) > 1 {
					body = tc.after
				}
				w.Header().Set("Content-Type", "text/plain")
				_, _ = w.Write([]byte(body))
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
			ratio := result.Sources[0].HitRatios["authn"]
			if ratio.Status != tc.wantStatus {
				t.Fatalf("ratio %s", ratio.Status)
			}
			if ratio.Value != nil && *ratio.Value != tc.wantRatio {
				t.Fatal("incorrect ratio")
			}
		})
	}
}

func TestMetricDerivationControls(t *testing.T) {
	for _, name := range []string{"missing-process", "zero-process", "scope", "labels", "gauge"} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				_, _ = w.Write([]byte(metricText))
			}))
			defer srv.Close()
			c, err := loadMetricCollector(metricsFixture(t, srv.URL))
			if err != nil {
				t.Fatal(err)
			}
			defer c.close()
			c.capture(context.Background(), false)
			c.capture(context.Background(), true)
			s := c.sources[0]
			// Inject only the comparison control under test after real fixture capture.
			*s.after.Observations[0].Value = 11
			*s.after.Observations[1].Value = 5
			switch name {
			case "missing-process":
				s.after.ProcessStartTime = nil
			case "zero-process":
				s.after.ProcessStartTime = nil // zero markers are rejected by parsing
			case "scope":
				s.public.ExclusiveTraffic = false
			case "labels":
				s.public.Selectors[1].Labels = map[string]string{"scope": "other"}
			case "gauge":
				s.public.Selectors[0].Type = "gauge"
			}
			out, status := c.results()
			if status.Status != "complete" {
				t.Fatal("derivation changed capture completeness")
			}
			if out.Sources[0].HitRatios["authn"].Value != nil {
				t.Fatal("invalid comparison derived a ratio")
			}
			if name == "gauge" && out.Sources[0].Derived[0].Status != "not-applicable" {
				t.Fatal("gauge delta fabricated")
			}
		})
	}
}
