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
)

type poolCredentialKey struct{}

func withPoolCredential(ctx context.Context, c *poolClient) context.Context {
	return context.WithValue(ctx, poolCredentialKey{}, c)
}
func selectedCredential(ctx context.Context) (*poolClient, error) {
	c, ok := ctx.Value(poolCredentialKey{}).(*poolClient)
	if !ok || c == nil {
		return nil, fmt.Errorf("pool credential selection missing")
	}
	return c, nil
}

type poolRPCCredentials struct{}

func (poolRPCCredentials) RequireTransportSecurity() bool { return true }
func (poolRPCCredentials) GetRequestMetadata(ctx context.Context, _ ...string) (map[string]string, error) {
	c, err := selectedCredential(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]string{"authorization": "Bearer " + c.token}, nil
}

type poolTransport struct{ base http.RoundTripper }

func (p *poolTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c, err := selectedCredential(req.Context())
	if err != nil {
		return nil, err
	}
	cloned := req.Clone(req.Context())
	cloned.Header.Set("Authorization", "Bearer "+c.token)
	return p.base.RoundTrip(cloned)
}
func (p *poolTransport) WrappedRoundTripper() http.RoundTripper { return p.base }
