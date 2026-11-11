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
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	pb "github.com/tektoncd/results/proto/v1alpha2/results_go_proto"
)

func writeToken(t *testing.T, dir, name, token string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadTokenPool(t *testing.T) {
	for _, tt := range []struct {
		name    string
		count   int
		token   string
		wantErr bool
	}{
		{name: "empty directory", wantErr: true},
		{name: "one", count: 1, token: "token"},
		{name: "thousand", count: 1000, token: "token"},
		{name: "too many", count: 1001, token: "token", wantErr: true},
		{name: "blank", count: 1, wantErr: true},
		{name: "duplicate", count: 2, token: "duplicate", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeToken(t, dir, "ignored.txt", "ignored")
			for i := 0; i < tt.count; i++ {
				token := tt.token
				if token == "token" {
					token = fmt.Sprintf("token-%d\n", i)
				}
				writeToken(t, dir, fmt.Sprintf("%04d.token", i), token)
			}
			pool, err := loadTokenPool(dir)
			if (err != nil) != tt.wantErr {
				t.Fatalf("loadTokenPool error = %v, want error %t", err, tt.wantErr)
			}
			if !tt.wantErr && len(pool.tokens) != tt.count {
				t.Fatalf("got %d tokens, want %d", len(pool.tokens), tt.count)
			}
		})
	}
	if _, err := loadTokenPool(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing directory accepted")
	}
}

func TestTokenPoolConcurrentRotation(t *testing.T) {
	pool := &tokenPool{tokens: []string{"a", "b", "c"}}
	var wg sync.WaitGroup
	var mu sync.Mutex
	counts := map[string]int{}
	for range 1000 {
		wg.Go(func() {
			for range 3 {
				token, err := pool.Token()
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				counts[token.AccessToken]++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	for _, token := range pool.tokens {
		if counts[token] != 1000 {
			t.Errorf("token %s used %d times, want 1000", token, counts[token])
		}
	}
}

func TestAPIClientsTokenRotation(t *testing.T) {
	for _, usePool := range []bool{false, true} {
		t.Run(fmt.Sprintf("pool=%t", usePool), func(t *testing.T) {
			var mu sync.Mutex
			var got []string
			record := func(token string) { mu.Lock(); got = append(got, token); mu.Unlock() }
			gs := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, _ any, _ *grpc.UnaryServerInfo, _ grpc.UnaryHandler) (any, error) {
				md, _ := metadata.FromIncomingContext(ctx)
				record(strings.Join(md.Get("authorization"), ","))
				return &pb.ListResultsResponse{}, nil
			}))
			defer gs.Stop()
			pb.RegisterResultsServer(gs, &pb.UnimplementedResultsServer{})
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
					gs.ServeHTTP(w, r)
					return
				}
				record(r.Header.Get("Authorization"))
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte("{}"))
			}))
			server.EnableHTTP2 = true
			server.StartTLS()
			defer server.Close()
			dir := t.TempDir()
			certPath := filepath.Join(dir, "ca.pem")
			cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
			if err := os.WriteFile(certPath, cert, 0o600); err != nil {
				t.Fatal(err)
			}
			writeToken(t, dir, "single", "single-token")
			cfg := ClientConfig{ServerAddress: server.URL, ServerName: "example.com", CertFile: certPath, TokenFile: filepath.Join(dir, "single")}
			wantCount := 1
			if usePool {
				writeToken(t, dir, "a.token", "token-a\n")
				writeToken(t, dir, "b.token", "token-b\n")
				cfg.TokensDir = dir
				cfg.TokenFile = filepath.Join(dir, "missing-fallback") // Pool overrides the single-token path.
				wantCount = 2
			}
			clients, err := NewAPIClients(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = clients.Close() }()
			if clients.TokenCount != wantCount {
				t.Fatalf("TokenCount = %d, want %d", clients.TokenCount, wantCount)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			// Each transport must rotate on its own; then verify the pool is shared.
			for _, useREST := range []bool{false, false, false, true, true, true} {
				if useREST {
					_, err = clients.REST.ListResults(ctx, &pb.ListResultsRequest{Parent: "ns"})
				} else {
					_, err = clients.GRPC.ListResults(ctx, &pb.ListResultsRequest{Parent: "ns"})
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if len(got) != 6 {
				t.Fatalf("got %d requests, want 6", len(got))
			}
			for i, token := range got {
				want := "Bearer single-token"
				if usePool {
					want = []string{"Bearer token-a", "Bearer token-b"}[i%2]
				}
				if token != want {
					t.Errorf("request %d authorization = %q, want %q", i, token, want)
				}
			}
		})
	}
}
