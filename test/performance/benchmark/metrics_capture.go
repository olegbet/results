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
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/tektoncd/results/test/performance/report"
)

const (
	maxMetricBody   int64 = 4 * 1024 * 1024
	maxMetricSeries       = 10000
)

var metricName = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
var labelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
var namedSignals = []string{"authn_hits", "authn_misses", "authz_hits", "authz_misses", "authn_entries", "authz_entries", "authn_evictions", "authz_evictions", "tokenreviews", "subjectaccessreviews"}

type metricSourceInput struct {
	ID               string                  `json:"id"`
	URL              string                  `json:"url"`
	InstanceID       string                  `json:"instance_id"`
	ExclusiveTraffic *bool                   `json:"exclusive_traffic"`
	ScopeEvidenceRef string                  `json:"scope_evidence_ref,omitempty"`
	Timeout          string                  `json:"timeout,omitempty"`
	Selectors        []report.MetricSelector `json:"selectors"`
	BearerTokenFile  string                  `json:"bearer_token_file,omitempty"`
	CAFile           string                  `json:"ca_file,omitempty"`
	ServerName       string                  `json:"server_name,omitempty"`
}
type metricConfigInput struct {
	SchemaVersion string              `json:"schema_version"`
	Sources       []metricSourceInput `json:"sources"`
}
type metricSource struct {
	public        report.MetricSource
	url           string
	token         string
	http          *http.Client
	before, after *report.CaptureAttempt
}
type metricCollector struct{ sources []*metricSource }

func reason(s string) *string { return &s }
func privateFile(base, path string) ([]byte, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(filepath.Dir(base), path)
	}
	return os.ReadFile(path) //nolint:gosec // explicit operator-supplied private input
}
func loadMetricCollector(path string) (collector *metricCollector, err error) {
	var cfg metricConfigInput
	if err := readStrictJSON(path, &cfg); err != nil {
		return nil, fmt.Errorf("metrics configuration: %w", err)
	}
	if cfg.SchemaVersion != "1" || len(cfg.Sources) == 0 || len(cfg.Sources) > 4 {
		return nil, fmt.Errorf("metrics configuration: invalid-header")
	}
	collector = &metricCollector{}
	prepared := collector
	defer func() {
		if err != nil {
			prepared.close()
		}
	}()
	ids := map[string]bool{}
	for i, in := range cfg.Sources {
		fail := func(code string) (*metricCollector, error) { return nil, fmt.Errorf("metrics source %d: %s", i, code) }
		if !publicID.MatchString(in.ID) || ids[in.ID] {
			return fail("invalid-or-duplicate-id")
		}
		ids[in.ID] = true
		fail = func(code string) (*metricCollector, error) {
			return nil, fmt.Errorf("metrics source %s: %s", in.ID, code)
		}
		if in.InstanceID == "" || in.ExclusiveTraffic == nil || (*in.ExclusiveTraffic && in.ScopeEvidenceRef == "") {
			return fail("missing-source-scope")
		}
		u, e := url.Parse(in.URL)
		if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || (u.Scheme != "https" && u.Scheme != "http") {
			return fail("invalid-url")
		}
		if u.Scheme == "http" {
			ip := net.ParseIP(u.Hostname())
			if u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
				return fail("unencrypted-remote-source")
			}
		}
		if in.Timeout == "" {
			in.Timeout = "5s"
		}
		timeout, e := time.ParseDuration(in.Timeout)
		if e != nil || timeout <= 0 || timeout > 30*time.Second {
			return fail("invalid-timeout")
		}
		if len(in.Selectors) == 0 || len(in.Selectors) > 32 {
			return fail("invalid-selector-count")
		}
		selections, selectorIDs, roles := map[string]bool{}, map[string]bool{}, map[string]bool{}
		for _, s := range in.Selectors {
			if !publicID.MatchString(s.ID) || selectorIDs[s.ID] || !metricName.MatchString(s.Metric) || s.Labels == nil || (s.Type != "counter" && s.Type != "gauge") {
				return fail("invalid-selector")
			}
			selectorIDs[s.ID] = true
			for name := range s.Labels {
				if !labelName.MatchString(name) {
					return fail("invalid-label")
				}
			}
			key, e := json.Marshal(struct {
				Metric string
				Labels map[string]string
			}{s.Metric, s.Labels})
			if e != nil || selections[string(key)] {
				return fail("duplicate-selection")
			}
			selections[string(key)] = true
			if s.Signal != "supporting" {
				found := false
				for _, role := range namedSignals {
					found = found || role == s.Signal
				}
				if !found || roles[s.Signal] {
					return fail("invalid-or-duplicate-signal")
				}
				roles[s.Signal] = true
				expectedType := "counter"
				if strings.HasSuffix(s.Signal, "_entries") {
					expectedType = "gauge"
				}
				if s.Type != expectedType {
					return fail("signal-type-mismatch")
				}
			}
		}
		tc := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: in.ServerName}
		if in.CAFile != "" {
			b, e := privateFile(path, in.CAFile)
			if e != nil {
				return fail("invalid-ca")
			}
			tc.RootCAs = x509.NewCertPool()
			if !tc.RootCAs.AppendCertsFromPEM(b) {
				return fail("invalid-ca")
			}
		}
		token := ""
		if in.BearerTokenFile != "" {
			b, e := privateFile(path, in.BearerTokenFile)
			if e != nil {
				return fail("invalid-credential")
			}
			token = strings.TrimSpace(string(b))
			if token == "" || strings.ContainsFunc(token, unicode.IsSpace) {
				return fail("invalid-credential")
			}
		}
		tr := &http.Transport{TLSClientConfig: tc, DisableCompression: true, ResponseHeaderTimeout: timeout, IdleConnTimeout: 30 * time.Second}
		hc := &http.Client{Transport: tr, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		collector.sources = append(collector.sources, &metricSource{public: report.MetricSource{ID: in.ID, InstanceID: in.InstanceID, ExclusiveTraffic: *in.ExclusiveTraffic, ScopeEvidenceRef: in.ScopeEvidenceRef, Timeout: in.Timeout, Selectors: in.Selectors}, url: in.URL, token: token, http: hc})
	}
	return collector, nil
}
func (c *metricCollector) close() {
	if c != nil {
		for _, s := range c.sources {
			s.http.CloseIdleConnections()
		}
	}
}
func (c *metricCollector) configuration() *report.CaptureConfig {
	cfg := &report.CaptureConfig{MaxSources: 4, MaxSelectors: 32, MaxTimeout: "30s", MaxBodyBytes: maxMetricBody, MaxSeries: maxMetricSeries}
	for _, s := range c.sources {
		cfg.Sources = append(cfg.Sources, s.public)
	}
	return cfg
}
func (c *metricCollector) capture(ctx context.Context, after bool) {
	if c == nil {
		return
	}
	for _, s := range c.sources {
		attempt := s.capture(ctx)
		if after {
			s.after = attempt
		} else {
			s.before = attempt
		}
	}
}
func (s *metricSource) capture(ctx context.Context) *report.CaptureAttempt {
	a := &report.CaptureAttempt{Status: "unavailable", StartedAt: time.Now().UTC().Format(time.RFC3339Nano), Reason: reason("request-failed")}
	defer func() { a.EndedAt = time.Now().UTC().Format(time.RFC3339Nano) }()
	for _, selector := range s.public.Selectors {
		a.Observations = append(a.Observations, report.MetricObservation{ID: selector.ID, Metric: selector.Metric, Type: selector.Type, Labels: selector.Labels, Status: "unavailable", Reason: reason("request-failed")})
	}
	unavailable := func(code string) *report.CaptureAttempt {
		a.Reason = reason(code)
		for i := range a.Observations {
			a.Observations[i].Reason = reason(code)
		}
		return a
	}
	timeout, _ := time.ParseDuration(s.public.Timeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return unavailable("request-failed")
	}
	req.Header.Set("Accept", "text/plain; version=0.0.4")
	req.Header.Set("Accept-Encoding", "identity")
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	res, err := s.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return unavailable("timeout")
		}
		return unavailable("request-failed")
	}
	defer func() { _ = res.Body.Close() }()
	a.HTTPStatus = res.StatusCode
	if res.StatusCode != http.StatusOK {
		return unavailable("http-status")
	}
	media, params, err := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if err != nil || media != "text/plain" || (params["version"] != "" && params["version"] != "0.0.4") {
		return unavailable("unsupported-format")
	}
	if encoding := res.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		return unavailable("unsupported-format")
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxMetricBody+1))
	if err != nil {
		if ctx.Err() != nil {
			return unavailable("timeout")
		}
		return unavailable("request-failed")
	}
	if int64(len(body)) > maxMetricBody {
		return unavailable("body-limit")
	}
	parser := expfmt.NewTextParser(model.LegacyValidation)
	families, err := parser.TextToMetricFamilies(bytes.NewReader(body))
	if err != nil {
		return unavailable("parse-failed")
	}
	series := 0
	for _, family := range families {
		seen := map[string]bool{}
		for _, metric := range family.Metric {
			labels := map[string]string{}
			for _, label := range metric.Label {
				if _, ok := labels[label.GetName()]; ok {
					return unavailable("duplicate-series")
				}
				labels[label.GetName()] = label.GetValue()
			}
			key, _ := json.Marshal(labels)
			if seen[string(key)] {
				return unavailable("duplicate-series")
			}
			seen[string(key)] = true
			switch family.GetType() {
			case dto.MetricType_HISTOGRAM:
				series += 2 + len(metric.GetHistogram().Bucket)
			case dto.MetricType_SUMMARY:
				series += 2 + len(metric.GetSummary().Quantile)
			default:
				series++
			}
			if series > maxMetricSeries {
				return unavailable("series-limit")
			}
		}
	}
	a.Status = "available"
	a.Reason = nil
	if family := families["process_start_time_seconds"]; family != nil && family.GetType() == dto.MetricType_GAUGE {
		for _, metric := range family.Metric {
			value := metric.GetGauge().GetValue()
			if len(metric.Label) == 0 && metric.TimestampMs == nil && finite(value) && value > 0 {
				a.ProcessStartTime = &value
			}
		}
	}
	for i, selector := range s.public.Selectors {
		obs := &a.Observations[i]
		obs.Reason = reason("metric-absent")
		family := families[selector.Metric]
		if family == nil {
			continue
		}
		expectedType := dto.MetricType_COUNTER
		if selector.Type == "gauge" {
			expectedType = dto.MetricType_GAUGE
		}
		if family.GetType() != expectedType {
			obs.Reason = reason("unsupported-type")
			continue
		}
		for _, metric := range family.Metric {
			labels := map[string]string{}
			for _, label := range metric.Label {
				labels[label.GetName()] = label.GetValue()
			}
			if !reflect.DeepEqual(labels, selector.Labels) {
				continue
			}
			value := metric.GetCounter().GetValue()
			if selector.Type == "gauge" {
				value = metric.GetGauge().GetValue()
			}
			if metric.TimestampMs != nil || !finite(value) || (selector.Type == "counter" && value < 0) {
				obs.Reason = reason("invalid-sample")
				continue
			}
			obs.Status = "available"
			obs.Value = &value
			obs.Reason = nil
		}
	}
	return a
}
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func deriveMetric(s *metricSource, index int) report.MetricDelta {
	selector := s.public.Selectors[index]
	out := report.MetricDelta{ID: selector.ID, Status: "unavailable", Reason: reason("incomplete-pair")}
	if selector.Type == "gauge" {
		out.Status = "not-applicable"
		out.Reason = reason("gauge-snapshot")
		return out
	}
	if s.before == nil || s.after == nil {
		return out
	}
	b, a := s.before.Observations[index], s.after.Observations[index]
	if b.Status != "available" || a.Status != "available" {
		return out
	}
	if s.before.ProcessStartTime == nil || s.after.ProcessStartTime == nil {
		out.Reason = reason("process-identity-unavailable")
		return out
	}
	if *s.before.ProcessStartTime != *s.after.ProcessStartTime {
		out.Status = "invalid"
		out.Reason = reason("process-changed")
		return out
	}
	if *a.Value < *b.Value {
		out.Status = "invalid"
		out.Reason = reason("counter-reset")
		return out
	}
	if !s.public.ExclusiveTraffic || s.public.ScopeEvidenceRef == "" {
		out.Reason = reason("scope-unverified")
		return out
	}
	delta := *a.Value - *b.Value
	out.Status = "available"
	out.Delta = &delta
	out.Reason = nil
	return out
}
func (c *metricCollector) results() (*report.MetricCapture, *report.CaptureStatus) {
	result := &report.MetricCapture{}
	availability := &report.CaptureStatus{Status: "complete", ReasonCodes: []string{}}
	expected, observed := 0, 0
	reasons := map[string]bool{}
	for _, s := range c.sources {
		out := report.CapturedSource{ID: s.public.ID, InstanceID: s.public.InstanceID, Before: s.before, After: s.after, Signals: map[string]report.MetricSignal{}, HitRatios: map[string]report.HitRatio{}}
		for _, role := range namedSignals {
			out.Signals[role] = report.MetricSignal{Status: "unavailable", Reason: reason("not-configured")}
		}
		mapping := map[string]int{}
		for i, selector := range s.public.Selectors {
			expected += 2
			for _, attempt := range []*report.CaptureAttempt{s.before, s.after} {
				if attempt != nil && attempt.Observations[i].Status == "available" {
					observed++
				} else {
					code := "incomplete-pair"
					if attempt != nil && attempt.Observations[i].Reason != nil {
						code = *attempt.Observations[i].Reason
					}
					if !reasons[code] {
						availability.ReasonCodes = append(availability.ReasonCodes, code)
						reasons[code] = true
					}
				}
			}
			delta := deriveMetric(s, i)
			out.Derived = append(out.Derived, delta)
			if selector.Signal != "supporting" {
				mapping[selector.Signal] = i
				signal := report.MetricSignal{SelectorID: reason(selector.ID), Status: delta.Status, Reason: delta.Reason}
				if selector.Type == "gauge" {
					signal.Status = "unavailable"
					signal.Reason = reason("incomplete-pair")
					if s.before != nil && s.after != nil && s.before.Observations[i].Status == "available" && s.after.Observations[i].Status == "available" {
						signal.Status = "available"
						signal.Reason = nil
					}
				}
				out.Signals[selector.Signal] = signal
			}
		}
		for _, group := range []string{"authn", "authz"} {
			ratio := report.HitRatio{Status: "unavailable", Reason: reason("not-configured")}
			hi, hasHit := mapping[group+"_hits"]
			mi, hasMiss := mapping[group+"_misses"]
			if hasHit && hasMiss {
				hit, miss := out.Derived[hi], out.Derived[mi]
				switch {
				case hit.Status == "invalid" || miss.Status == "invalid":
					ratio.Status = "invalid"
					ratio.Reason = reason("component-invalid")
				case !reflect.DeepEqual(s.public.Selectors[hi].Labels, s.public.Selectors[mi].Labels):
					ratio.Reason = reason("scope-mismatch")
				case hit.Delta == nil || miss.Delta == nil:
					ratio.Reason = reason("incomplete-pair")
				case *hit.Delta+*miss.Delta == 0:
					ratio.Reason = reason("zero-denominator")
				default:
					value := *hit.Delta / (*hit.Delta + *miss.Delta)
					if !finite(value) || !finite(*hit.Delta+*miss.Delta) {
						ratio.Status = "invalid"
						ratio.Reason = reason("invalid-sample")
					} else {
						ratio.Status = "available"
						ratio.Value = &value
						ratio.Reason = nil
					}
				}
			}
			out.HitRatios[group] = ratio
		}
		result.Sources = append(result.Sources, out)
	}
	if observed == 0 {
		availability.Status = "unavailable"
	} else if observed < expected {
		availability.Status = "partial"
	}
	return result, availability
}
