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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMetricMissingZeroDuplicateTimestamp(t *testing.T) {
	for _, tc := range []struct{ body, status string }{{"# TYPE cache_hits_total counter\ncache_hits_total 0\n", "available"}, {"# TYPE cache_hits_total counter\ncache_hits_total{extra=\"unselected\"} 3\n", "unavailable"}, {"# TYPE cache_hits_total counter\ncache_hits_total 7 123\n", "unavailable"}, {"# TYPE cache_hits_total counter\ncache_hits_total 3\ncache_hits_total 4\n", "unavailable"}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(tc.body))
		}))
		c, err := loadMetricCollector(metricsFixture(t, srv.URL))
		if err != nil {
			t.Fatal(err)
		}
		c.capture(context.Background(), false)
		result, _ := c.results()
		if result.Sources[0].Before.Observations[0].Status != tc.status {
			t.Fatal("sample availability incorrect")
		}
		c.close()
		srv.Close()
	}
}

func TestMetricParseSeriesBoundsAndTypes(t *testing.T) {
	var series strings.Builder
	series.WriteString("# TYPE unrelated gauge\n")
	for i := 0; i < 10001; i++ {
		fmt.Fprintf(&series, "unrelated{id=\"%d\"} 1\n", i)
	}
	for _, tc := range []struct{ name, body, want string }{{"series", series.String(), "series-limit"}, {"negative", "# TYPE cache_hits_total counter\ncache_hits_total -1\n", "invalid-sample"}, {"summary", "# TYPE cache_hits_total summary\ncache_hits_total_sum 1\ncache_hits_total_count 1\n", "unsupported-type"}, {"nonfinite", "# TYPE cache_hits_total counter\ncache_hits_total NaN\n", "invalid-sample"}, {"histogram", "# TYPE cache_hits_total histogram\ncache_hits_total_count 1\ncache_hits_total_sum 1\n", "unsupported-type"}, {"untyped", "cache_hits_total 1\n", "unsupported-type"}} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c, err := loadMetricCollector(metricsFixture(t, srv.URL))
			if err != nil {
				t.Fatal(err)
			}
			defer c.close()
			c.capture(context.Background(), false)
			got, _ := c.results()
			if got.Sources[0].Before.Observations[0].Reason == nil || *got.Sources[0].Before.Observations[0].Reason != tc.want {
				t.Fatal("invalid exposition treated as valid sample")
			}
		})
	}
}
