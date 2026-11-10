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
	"sync/atomic"
	"testing"
	"time"

	"github.com/tektoncd/results/test/performance/metrics"
)

func TestPoolDistributionAndAccounting(t *testing.T) {
	for _, workers := range []int{8, 1000} {
		p, err := loadTokenPool(poolFixture(t, 1000), 0)
		if err != nil {
			t.Fatal(err)
		}
		state := newRunState(p)
		ctx := withRunState(context.Background(), state)
		var counts [1000]atomic.Int64
		m := runIndexed(ctx, workers, 0, 1000, func(ctx context.Context, i int, m *metrics.MetricSet) {
			counts[i].Add(1)
			_ = observe(ctx, m, "test", func() error { return nil })
		})
		for i := range counts {
			if counts[i].Load() != 1 {
				t.Fatal("index omitted or duplicated")
			}
		}
		clients := state.clientSnapshots()
		var ops int64
		for _, c := range clients {
			if c.LogicalStarted != 1 || c.LogicalCompleted != 1 || c.APISuccesses != 1 {
				t.Fatal("incorrect per-client coverage")
			}
			ops += c.TotalOps
		}
		if ops != m.Snapshot().TotalOps {
			t.Fatal("counter reconciliation failed")
		}
	}
}
func TestExecutionReasonsAndJoin(t *testing.T) {
	state := newRunState(nil)
	ctx, cancel := context.WithCancel(withRunState(context.Background(), state))
	var active atomic.Int64
	runIndexed(ctx, 8, 0, 100, func(context.Context, int, *metrics.MetricSet) {
		active.Add(1)
		cancel()
		time.Sleep(time.Millisecond)
		active.Add(-1)
	})
	if active.Load() != 0 || state.execution.ReasonCodes[0] != "run-canceled" {
		t.Fatal("snapshot before join or incorrect execution cause")
	}
	state = newRunState(nil)
	runTimed(withRunState(context.Background(), state), 2, time.Millisecond, func(context.Context, int, int, *metrics.MetricSet) {})
	if state.execution.ReasonCodes[0] != "duration-limit-reached" {
		t.Fatal("normal duration became interruption")
	}
}

func TestPoolTimedDispatchSkewAndDeadline(t *testing.T) {
	p, err := loadTokenPool(poolFixture(t, 1000), 0)
	if err != nil {
		t.Fatal(err)
	}
	state := newRunState(p)
	ctx, cancel := context.WithCancel(withRunState(context.Background(), state))
	defer cancel()
	runTimed(ctx, 8, time.Second, func(_ context.Context, worker, index int, _ *metrics.MetricSet) {
		if worker == 0 {
			time.Sleep(time.Millisecond)
		}
		if index >= 2000 {
			cancel()
		}
	})
	for _, c := range state.clientSnapshots() {
		if c.LogicalStarted == 0 {
			t.Fatal("worker-local selection omitted a client")
		}
	}
	state = newRunState(nil)
	ctx, cancel = context.WithTimeout(withRunState(context.Background(), state), time.Millisecond)
	defer cancel()
	runTimed(ctx, 2, time.Second, func(ctx context.Context, _ int, _ int, _ *metrics.MetricSet) { <-ctx.Done() })
	if state.execution.ReasonCodes[0] != "run-deadline-exceeded" {
		t.Fatal("enclosing deadline confused with configured duration")
	}
}
