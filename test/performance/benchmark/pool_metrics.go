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
	"sort"
	"sync"
	"time"

	"github.com/tektoncd/results/test/performance/metrics"
	"github.com/tektoncd/results/test/performance/report"
)

type clientCounter struct {
	mu    sync.Mutex
	value report.ClientMetrics
}
type runStateKey struct{}
type selectedCounterKey struct{}
type runState struct {
	pool        *tokenPool
	counters    []*clientCounter
	execution   report.Execution
	mu          sync.Mutex
	cases       *accessCases
	scenario    *report.Scenario
	collector   *metricCollector
	transport   string
	caseResults map[string]report.CaseResult
}

func newRunState(pool *tokenPool) *runState {
	s := &runState{pool: pool, caseResults: map[string]report.CaseResult{}}
	if pool != nil {
		for _, c := range pool.clients {
			s.counters = append(s.counters, &clientCounter{value: report.ClientMetrics{ID: c.ID, ByOp: map[string]report.OpCounts{}}})
		}
	}
	return s
}
func withRunState(ctx context.Context, s *runState) context.Context {
	return context.WithValue(ctx, runStateKey{}, s)
}
func stateFrom(ctx context.Context) *runState { s, _ := ctx.Value(runStateKey{}).(*runState); return s }
func (s *runState) begin(ctx context.Context, index int) context.Context {
	if s == nil || len(s.counters) == 0 {
		return ctx
	}
	i := index % len(s.counters)
	c := s.counters[i]
	c.mu.Lock()
	c.value.LogicalStarted++
	c.mu.Unlock()
	if s.pool != nil {
		ctx = withPoolCredential(ctx, &s.pool.clients[i])
	}
	return context.WithValue(ctx, selectedCounterKey{}, c)
}
func endLogical(ctx context.Context) {
	if c, ok := ctx.Value(selectedCounterKey{}).(*clientCounter); ok && ctx.Err() == nil {
		c.mu.Lock()
		c.value.LogicalCompleted++
		c.mu.Unlock()
	}
}
func (s *runState) finish(parent context.Context, reason string) {
	if s == nil {
		return
	}
	status := "completed"
	if parent.Err() != nil {
		status = "interrupted"
		reason = "run-canceled"
		if parent.Err() == context.DeadlineExceeded {
			reason = "run-deadline-exceeded"
		}
	}
	s.execution = report.Execution{Status: status, ReasonCodes: []string{reason}}
}
func recordClient(ctx context.Context, op string, err error, api bool) {
	c, ok := ctx.Value(selectedCounterKey{}).(*clientCounter)
	if !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	v := &c.value
	v.TotalOps++
	code := metrics.Classify(err)
	counts := v.ByOp[op]
	if counts.ExitCodes == nil {
		counts.ExitCodes = map[string]int64{}
	}
	counts.Count++
	counts.ExitCodes[code]++
	if err != nil {
		v.TotalErrors++
		counts.Errors++
	}
	if api {
		v.APIOps++
		if err == nil {
			v.APISuccesses++
		}
	} else if err != nil {
		v.LocalErrors++
	}
	v.ByOp[op] = counts
}
func localObserve(ctx context.Context, m *metrics.MetricSet, op string, err error) {
	m.Observe(op, 0, err)
	recordClient(ctx, op, err, false)
}
func (s *runState) clientSnapshots() []report.ClientMetrics {
	out := make([]report.ClientMetrics, 0, len(s.counters))
	for _, c := range s.counters {
		c.mu.Lock()
		v := c.value
		v.ByOp = map[string]report.OpCounts{}
		for op, count := range c.value.ByOp {
			cloned := count
			cloned.ExitCodes = map[string]int64{}
			for code, n := range count.ExitCodes {
				cloned.ExitCodes[code] = n
			}
			v.ByOp[op] = cloned
		}
		out = append(out, v)
		c.mu.Unlock()
	}
	return out
}
func (s *runState) validation(snap metrics.Snapshot) *report.Validation {
	v := &report.Validation{Execution: s.execution, MeasurementWindow: report.MeasurementWindow{StartedAt: snap.Start.UTC().Format(time.RFC3339Nano), EndedAt: snap.Start.Add(snap.Duration).UTC().Format(time.RFC3339Nano)}, Responses: report.Responses{Status: "not-requested"}}
	if len(s.counters) > 0 {
		coverage := &report.Coverage{Status: "complete"}
		for _, c := range s.clientSnapshots() {
			if c.APIOps > 0 {
				coverage.ExercisedClients++
			}
			if c.APISuccesses > 0 {
				coverage.SuccessfulClients++
			}
		}
		if coverage.ExercisedClients < len(s.counters) {
			coverage.Status = "partial"
		}
		v.Coverage = coverage
	}
	if s.cases != nil {
		required := len(s.counters) * len(s.cases.Cases)
		if s.transport == "both" {
			required *= 2
		}
		v.Coverage.RequiredPairs = required
		keys := make([]string, 0, len(s.caseResults))
		for key := range s.caseResults {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			entry := s.caseResults[key]
			v.CaseResults = append(v.CaseResults, entry)
			v.Responses.Mismatches += entry.Mismatches
			v.Coverage.AttemptedPairs++
			if entry.LogicalCompleted > 0 && entry.LogicalCompleted == entry.LogicalStarted {
				v.Coverage.CompletedPairs++
			}
		}
		v.Responses.Status = "passed"
		if v.Coverage.CompletedPairs < required || v.Coverage.CompletedPairs < v.Coverage.AttemptedPairs {
			v.Responses.Status = "incomplete"
			v.Coverage.Status = "partial"
		}
		if v.Responses.Mismatches > 0 {
			v.Responses.Status = "failed"
		}
	}
	return v
}

func (s *runState) beforeMeasurement(ctx context.Context) {
	if s != nil && s.collector != nil {
		s.collector.capture(ctx, false)
	}
}
func (s *runState) afterMeasurement(ctx context.Context) {
	if s != nil && s.collector != nil {
		final, cancel := context.WithTimeout(context.WithoutCancel(ctx), 120*time.Second)
		defer cancel()
		s.collector.capture(final, true)
		s.collector.close()
	}
}
func (s *runState) sharedDispatch() bool { return s != nil && (s.pool != nil || s.cases != nil) }
func (s *runState) close() {
	if s != nil && s.collector != nil {
		s.collector.close()
	}
}
