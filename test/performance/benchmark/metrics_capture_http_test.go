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
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const metricText = "# TYPE process_start_time_seconds gauge\nprocess_start_time_seconds 123\n# TYPE cache_hits_total counter\ncache_hits_total 7\n# TYPE cache_misses_total counter\ncache_misses_total 3\n"

func TestMetricHTTPFailuresAndSelection(t *testing.T) {
	for _, code := range []int{200, 401, 500} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" {
					t.Error("workload credential inherited")
				}
				if r.Header.Get("Accept-Encoding") != "identity" {
					t.Error("compression enabled")
				}
				w.Header().Set("Content-Type", "text/plain; version=0.0.4")
				w.WriteHeader(code)
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
			result, availability := c.results()
			if code == 200 {
				if availability.Status != "complete" || result.Sources[0].Derived[0].Delta == nil {
					t.Fatal("valid samples unavailable")
				}
			} else if availability.Status != "unavailable" {
				t.Fatal("failed endpoint presented as data")
			}
			b, _ := json.Marshal(result)
			if strings.Contains(string(b), srv.URL) {
				t.Fatal("private URL leaked")
			}
		})
	}
}

func TestMetricCaptureBoundsAndPartialRetention(t *testing.T) {
	for _, tc := range []struct {
		name, body, encoding, contentType, reason string
		code                                      int
	}{
		{name: "redirect", code: 302, contentType: "text/plain", reason: "http-status"},
		{name: "compressed", body: metricText, code: 200, encoding: "gzip", contentType: "text/plain", reason: "unsupported-format"},
		{name: "openmetrics", body: metricText, code: 200, contentType: "application/openmetrics-text", reason: "unsupported-format"},
		{name: "body-limit", body: strings.Repeat("x", int(maxMetricBody)+1), code: 200, contentType: "text/plain", reason: "body-limit"},
		{name: "parse-error", body: "private-invalid-\\secret\n", code: 200, contentType: "text/plain", reason: "parse-failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.Header().Set("Content-Encoding", tc.encoding)
				w.Header().Set("Location", "https://invalid.test/private")
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c, err := loadMetricCollector(metricsFixture(t, srv.URL))
			if err != nil {
				t.Fatal(err)
			}
			defer c.close()
			c.capture(context.Background(), false)
			result, _ := c.results()
			if *result.Sources[0].Before.Reason != tc.reason {
				t.Fatal("incorrect safe failure reason")
			}
		})
	}
	for _, failFirst := range []bool{true, false} {
		var attempts int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			attempts++
			if (attempts == 1) == failFirst {
				w.WriteHeader(500)
				return
			}
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(metricText))
		}))
		c, err := loadMetricCollector(metricsFixture(t, srv.URL))
		if err != nil {
			t.Fatal(err)
		}
		c.capture(context.Background(), false)
		c.capture(context.Background(), true)
		result, status := c.results()
		if status.Status != "partial" || len(result.Sources) != 1 {
			t.Fatal("counterpart failure lost samples")
		}
		c.close()
		srv.Close()
	}
}

func TestMetricHTTPSCredentialAndTimeout(t *testing.T) {
	const private = "synthetic-collector-secret"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+private {
			w.WriteHeader(401)
			return
		}
		if r.Header.Get("Accept-Encoding") != "identity" || r.Header.Get("Accept") != "text/plain; version=0.0.4" {
			t.Error("incorrect format request")
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(metricText))
	}))
	defer srv.Close()
	path := metricsFixture(t, srv.URL)
	dir := filepath.Dir(path)
	_ = os.WriteFile(filepath.Join(dir, "ca.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0600)
	_ = os.WriteFile(filepath.Join(dir, "bearer"), []byte(private), 0600)
	var doc metricConfigInput
	b, _ := os.ReadFile(path)
	_ = json.Unmarshal(b, &doc)
	doc.Sources[0].CAFile = "ca.pem"
	doc.Sources[0].BearerTokenFile = "bearer"
	doc.Sources[0].ServerName = "example.com"
	b, _ = json.Marshal(doc)
	_ = os.WriteFile(path, b, 0600)
	c, err := loadMetricCollector(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	c.capture(context.Background(), false)
	if c.sources[0].before.Status != "available" {
		t.Fatalf("trusted TLS capture: %v", c.sources[0].before.Reason)
	}
	// A bad server name fails without projecting TLS error strings or URLs.
	c.sources[0].http.Transport.(*http.Transport).TLSClientConfig.ServerName = "wrong.invalid"
	c.close()
	c.capture(context.Background(), true)
	if c.sources[0].after.Status != "unavailable" {
		t.Fatal("accepted wrong TLS identity")
	}
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer stalled.Close()
	c2, err := loadMetricCollector(metricsFixture(t, stalled.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer c2.close()
	c2.sources[0].public.Timeout = "20ms"
	start := time.Now()
	c2.capture(context.Background(), false)
	if time.Since(start) > time.Second || *c2.sources[0].before.Reason != "timeout" {
		t.Fatal("unbounded stalled read")
	}
}

func TestMetricSourceOrderAndNoProxyInheritance(t *testing.T) {
	var order []string
	server := func(id string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			order = append(order, id)
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(metricText))
		}))
	}
	first, second := server("first"), server("second")
	defer first.Close()
	defer second.Close()
	path := metricsFixture(t, first.URL)
	b, _ := os.ReadFile(path)
	var cfg metricConfigInput
	_ = json.Unmarshal(b, &cfg)
	other := cfg.Sources[0]
	other.ID = "second"
	other.URL = second.URL
	cfg.Sources = append(cfg.Sources, other)
	b, _ = json.Marshal(cfg)
	_ = os.WriteFile(path, b, 0600)
	t.Setenv("HTTP_PROXY", "http://private-invalid-proxy.invalid")
	c, err := loadMetricCollector(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	for _, source := range c.sources {
		if source.http.Transport.(*http.Transport).Proxy != nil {
			t.Fatal("proxy inherited")
		}
	}
	c.capture(context.Background(), false)
	c.capture(context.Background(), true)
	if strings.Join(order, ",") != "first,second,first,second" {
		t.Fatal("sources not sequential or duplicated")
	}
}
