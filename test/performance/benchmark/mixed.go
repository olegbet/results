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
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/tektoncd/results/proto/v1alpha2/results_go_proto"
	"github.com/tektoncd/results/test/performance/generator"
	"github.com/tektoncd/results/test/performance/metrics"
)

// mixedParams tunes the read/write split of a mixed run.
type mixedParams struct {
	Concurrency int
	Duration    time.Duration
	ReadRatio   int
	WriteRatio  int
	PageSize    int32
}

// runMixed drives writers over the live UID range and readers over the seed range
// in parallel. The ranges are disjoint by construction (see dataset.go), so reads
// return stable golden answers uncontaminated by concurrent writes, and writers
// never contend on the same Result. Writers pull distinct live indices via an
// atomic counter (each index, and thus each Result, handled once).
func runMixed(ctx context.Context, gc pb.ResultsClient, live, seed *generator.Generator, p mixedParams) *metrics.MetricSet {
	stateFrom(ctx).beforeMeasurement(ctx)
	writers, readers := splitWorkers(p.Concurrency, p.ReadRatio, p.WriteRatio)

	runCtx := ctx
	if p.Duration > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, p.Duration)
		defer cancel()
	}

	readerCtx, stopReaders := context.WithCancel(runCtx)
	defer stopReaders()

	var writerWG, readerWG sync.WaitGroup

	total := metrics.NewMetricSet()
	total.Start()

	sets := make([]*metrics.MetricSet, 0, writers+readers)
	var mu sync.Mutex
	addSet := func(s *metrics.MetricSet) {
		mu.Lock()
		sets = append(sets, s)
		mu.Unlock()
	}

	// Writers: dispatch distinct live indices until exhausted or time is up.
	var nextIndex int64 = -1
	liveCount := live.Count()
	for w := 0; w < writers; w++ {
		writerWG.Add(1)
		go func() {
			defer writerWG.Done()
			ws := metrics.NewMetricSet()
			addSet(ws)
			for runCtx.Err() == nil {
				i := int(atomic.AddInt64(&nextIndex, 1))
				if i >= liveCount {
					return
				}
				opCtx := stateFrom(ctx).begin(runCtx, i)
				inst, err := live.At(i)
				if err != nil {
					localObserve(opCtx, ws, "generate", err)
					endLogical(opCtx)
					continue
				}
				storeInstance(opCtx, gc, inst, updateCountFor(i), ws)
				endLogical(opCtx)
			}
		}()
	}

	// Readers: loop the query mix over the seed namespaces until time is up.
	queries := defaultQueries()
	namespaces := seed.Config().Namespaces
	l := grpcLister{c: gc}
	var readDispatch atomic.Int64
	for r := 0; r < readers; r++ {
		readerWG.Add(1)
		go func(readerID int) {
			defer readerWG.Done()
			rs := metrics.NewMetricSet()
			addSet(rs)
			for iter := 0; readerCtx.Err() == nil; iter++ {
				index := iter
				if stateFrom(ctx).sharedDispatch() {
					index = int(readDispatch.Add(1) - 1)
				}
				opCtx := stateFrom(ctx).begin(readerCtx, index)
				q := queries[index%len(queries)]
				ns := namespaces[(readerID+iter)%len(namespaces)]
				runQuery(opCtx, l, q, ns, p.PageSize, rs)
				endLogical(opCtx)
			}
		}(r)
	}

	writerWG.Wait()

	if p.Duration == 0 {
		stopReaders()
	}

	readerWG.Wait()
	reason := "count-limit-reached"
	if p.Duration > 0 {
		reason = "duration-limit-reached"
	}
	stateFrom(ctx).finish(ctx, reason)
	total.Stop()
	for _, s := range sets {
		total.Merge(s)
	}
	stateFrom(ctx).afterMeasurement(ctx)
	return total
}

// splitWorkers divides a worker budget between readers and writers by ratio,
// guaranteeing at least one of each. total must be at least 2, as validated by
// newMixedCmd before starting workers.
func splitWorkers(total, readRatio, writeRatio int) (writers, readers int) {
	sum := readRatio + writeRatio
	if sum <= 0 {
		writers = total / 2
		return writers, total - writers
	}
	writers = total * writeRatio / sum
	if writers < 1 {
		writers = 1
	}
	if writers >= total {
		writers = total - 1
	}
	return writers, total - writers
}
