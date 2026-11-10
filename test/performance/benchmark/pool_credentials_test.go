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
	"os"
	"strings"
	"sync"
	"testing"

	pb "github.com/tektoncd/results/proto/v1alpha2/results_go_proto"
)

func TestPoolConcurrentTransportIsolation(t *testing.T) {
	path := poolFixture(t, 16)
	p, err := loadTokenPool(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	seen := map[string]int{}
	args := apiFixture(t, queryResultsServer{}, func(r *http.Request) { mu.Lock(); seen[r.Header.Get("Authorization")]++; mu.Unlock() })
	c, err := NewAPIClients(ClientConfig{ServerAddress: args[1], ServerName: args[3], CertFile: args[5], pool: p})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var wg sync.WaitGroup
	for i := 0; i < 128; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := withPoolCredential(context.Background(), &p.clients[i%16])
			if (i/16)%2 == 0 {
				_, err := c.GRPC.ListResults(ctx, &pb.ListResultsRequest{Parent: "ns-00"})
				if err != nil {
					t.Error(err)
				}
			} else {
				_, err := c.REST.ListResults(ctx, &pb.ListResultsRequest{Parent: "ns-00"})
				if err != nil {
					t.Error(err)
				}
			}
		}(i)
	}
	wg.Wait()
	for _, entry := range p.clients {
		if seen["Bearer "+entry.token] != 8 {
			t.Fatal("lost or switched selected credentials")
		}
	}
	if _, err := c.REST.ListResults(context.Background(), &pb.ListResultsRequest{}); err == nil {
		t.Fatal("missing selection fell back")
	}
	if _, err := c.GRPC.ListResults(context.Background(), &pb.ListResultsRequest{}); err == nil {
		t.Fatal("missing grpc selection fell back")
	}
	_ = os.WriteFile(path, []byte("changed"), 0600)
	for token := range seen {
		if !strings.HasPrefix(token, "Bearer synthetic-") {
			t.Fatal("unexpected authorization")
		}
	}
}
