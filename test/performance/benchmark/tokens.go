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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"golang.org/x/oauth2"
)

// tokenPool shares a round-robin identity pool across gRPC and REST requests.
// Tokens are loaded once, so disk I/O is outside the measured workload.
type tokenPool struct {
	tokens []string
	next   atomic.Uint64
}

func loadTokenPool(dir string) (*tokenPool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading token directory: %w", err)
	}
	pool := &tokenPool{}
	seen := make(map[string]bool)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".token") {
			continue
		}
		if len(pool.tokens) == 1000 {
			return nil, fmt.Errorf("token pool must contain at most 1000 .token files")
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name())) //nolint:gosec // operator-provided token directory
		if err != nil {
			return nil, fmt.Errorf("reading token file %q: %w", entry.Name(), err)
		}
		token := strings.TrimSpace(string(data))
		if token == "" || seen[token] {
			return nil, fmt.Errorf("token file %q is empty or duplicates another token", entry.Name())
		}
		seen[token] = true
		pool.tokens = append(pool.tokens, token)
	}
	if len(pool.tokens) == 0 {
		return nil, fmt.Errorf("token directory must contain at least one .token file")
	}
	return pool, nil
}

func (p *tokenPool) Token() (*oauth2.Token, error) {
	index := (p.next.Add(1) - 1) % uint64(len(p.tokens))
	return &oauth2.Token{AccessToken: p.tokens[index], TokenType: "Bearer"}, nil
}
