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
	"errors"
	"testing"

	"github.com/tektoncd/results/test/performance/metrics"
)

func TestPoolLocalErrorsReconcile(t *testing.T) {
	p, err := loadTokenPool(poolFixture(t, 3), 0)
	if err != nil {
		t.Fatal(err)
	}
	s := newRunState(p)
	ctx := withRunState(context.Background(), s)
	m := runIndexed(ctx, 8, 0, 2, func(ctx context.Context, _ int, m *metrics.MetricSet) {
		localObserve(ctx, m, "generate", errors.New("synthetic-local"))
	})
	var ops, errs int64
	for _, c := range s.clientSnapshots() {
		ops += c.TotalOps
		errs += c.TotalErrors
		if c.APIOps != 0 || c.APISuccesses != 0 {
			t.Fatal("local failure treated as API activity")
		}
	}
	if ops != m.Snapshot().TotalOps || errs != m.Snapshot().TotalError || len(s.clientSnapshots()) != 3 {
		t.Fatal("partial counter reconciliation failed")
	}
}
