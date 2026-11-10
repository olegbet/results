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
	"os"

	"github.com/tektoncd/results/test/performance/report"

	"github.com/spf13/cobra"
)

// RunConfig holds the flags shared by every benchmark subcommand. Endpoint and
// auth flags default from the same environment variables the e2e suite uses so a
// benchmark run drops into an existing kind setup with no extra configuration.
type RunConfig struct {
	// Connection / auth.
	ServerAddress     string
	ServerName        string
	CertFile          string
	TokenFile         string
	TokenPoolFile     string
	AccessCasesFile   string
	RunContextFile    string
	MetricsConfigFile string
	DatasetCount      int
	datasetCountSet   bool
	pool              *tokenPool
	runtime           *runState

	// Workload shape.
	Concurrency int
	Count       int
	DurationSec int

	// Dataset (shared by generator, loader, and query golden answers).
	Seed           int64
	DatasetVersion string
	Dataset        string
	Namespaces     int
	ChildMin       int
	ChildMax       int

	// Reporting.
	Tier        string
	OutputPath  string
	DBBackend   string
	ServerImage string
}

// bindCommon registers the shared flags on cmd and wires their defaults from the
// environment. Subcommands add their own flags (transport, ratios) on top.
func bindCommon(cmd *cobra.Command, cfg *RunConfig) {
	f := cmd.Flags()
	if cmd.Use != "dataset" {
		f.StringVar(&cfg.TokenPoolFile, "token-pool", "", "versioned private token-pool manifest")
		f.StringVar(&cfg.RunContextFile, "run-context", "", "versioned cache comparison context")
		f.StringVar(&cfg.MetricsConfigFile, "metrics-config", "", "direct before/after metrics source configuration")
		if cmd.Use == "query" {
			f.StringVar(&cfg.AccessCasesFile, "access-cases", "", "explicit query access-case file")
			f.IntVar(&cfg.DatasetCount, "dataset-count", 0, "query seed count, independent of repetitions")
		}
		cmd.PreRunE = func(cmd *cobra.Command, _ []string) error { return cfg.prepare(cmd) }
	}
	f.StringVar(&cfg.ServerAddress, "server-addr", envOr("API_SERVER_ADDR", "https://localhost:8080"), "API server address (env API_SERVER_ADDR)")
	f.StringVar(&cfg.ServerName, "server-name", envOr("API_SERVER_NAME", "tekton-results-api-service.tekton-pipelines.svc.cluster.local"), "TLS server name (env API_SERVER_NAME)")
	f.StringVar(&cfg.CertFile, "cert", os.Getenv("SSL_CERT_FILE"), "CA certificate file; empty skips verification (env SSL_CERT_FILE)")
	f.StringVar(&cfg.TokenFile, "token", os.Getenv("SA_TOKEN_FILE"), "bearer token file (env SA_TOKEN_FILE)")

	f.IntVar(&cfg.Concurrency, "concurrency", 8, "number of concurrent workers")
	f.IntVar(&cfg.Count, "count", 1000, "number of top-level PipelineRuns (query: complete paginated queries) for count-bounded runs")
	f.IntVar(&cfg.DurationSec, "duration", 0, "query/mixed run duration in seconds (0 = count-bounded)")

	f.Int64Var(&cfg.Seed, "seed", 42, "deterministic generator seed")
	f.StringVar(&cfg.DatasetVersion, "dataset-version", "seed-small-v1", "dataset content version tag")
	f.StringVar(&cfg.Dataset, "dataset", "seed-small", "named dataset tier")
	f.IntVar(&cfg.Namespaces, "namespaces", 50, "number of namespaces to spread runs across")
	f.IntVar(&cfg.ChildMin, "child-min", 2, "minimum child TaskRuns per PipelineRun")
	f.IntVar(&cfg.ChildMax, "child-max", 15, "maximum child TaskRuns per PipelineRun")

	f.StringVar(&cfg.Tier, "tier", "tier1", "environment tier label for the report")
	f.StringVar(&cfg.OutputPath, "output", "", "report output path; empty writes /tmp/<timestamp>-<commit>.json")
	f.StringVar(&cfg.DBBackend, "db-type", envOr("BENCH_DB_TYPE", "local"), "database backend label: local|external (env BENCH_DB_TYPE)")
	f.StringVar(&cfg.ServerImage, "server-image", os.Getenv("BENCH_SERVER_IMAGE"), "server image tag for the report (env BENCH_SERVER_IMAGE)")
}

// clientConfig extracts the connection settings.
func (c *RunConfig) clientConfig() ClientConfig {
	return ClientConfig{
		ServerAddress: c.ServerAddress,
		ServerName:    c.ServerName,
		CertFile:      c.CertFile,
		TokenFile:     c.TokenFile,
		pool:          c.pool,
	}
}

// envOr returns the environment variable value or a fallback.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func (c *RunConfig) runContext(ctx context.Context) context.Context {
	if c.runtime != nil {
		return withRunState(ctx, c.runtime)
	}
	return ctx
}

func (c *RunConfig) prepare(cmd *cobra.Command) error {
	if cmd.Use == "query" {
		c.datasetCountSet = cmd.Flags().Changed("dataset-count")
		if c.datasetCountSet && c.DatasetCount <= 0 {
			return fmt.Errorf("dataset-count must be positive")
		}
	}
	var cases *accessCases
	if c.AccessCasesFile != "" {
		parsed, err := loadAccessCases(c.AccessCasesFile)
		if err != nil {
			return err
		}
		cases = parsed
	}
	if c.TokenPoolFile != "" {
		if cmd.Flags().Changed("token") {
			return fmt.Errorf("--token and --token-pool conflict")
		}
		pool, err := loadTokenPool(c.TokenPoolFile, readDuration(c))
		if err != nil {
			return err
		}
		c.pool = pool
	}
	active := c.pool != nil || cases != nil || c.RunContextFile != "" || c.MetricsConfigFile != "" || c.datasetCountSet
	if !active {
		return nil
	}
	if (c.pool != nil || cases != nil) && (c.Concurrency < 1 || c.DurationSec < 0) {
		return fmt.Errorf("pool/case mode requires positive concurrency and nonnegative duration")
	}
	state := newRunState(c.pool)
	state.cases = cases
	if c.pool == nil && (cases != nil || c.RunContextFile != "") {
		state.counters = []*clientCounter{{value: report.ClientMetrics{ID: "single", ByOp: map[string]report.OpCounts{}}}}
	}
	if c.RunContextFile != "" {
		scenario, err := loadRunContext(c.RunContextFile, c)
		if err != nil {
			return err
		}
		state.scenario = scenario
	}
	if cases != nil {
		if state.scenario == nil {
			state.scenario = &report.Scenario{}
		}
		state.scenario.AccessCasesFingerprint = cases.fingerprint
		if err := validateCaseClients(c.pool, cases); err != nil {
			return err
		}
	}
	if c.pool != nil && cases == nil {
		for _, client := range c.pool.clients {
			if client.IntentionallyInvalid {
				return fmt.Errorf("invalid credentials require explicit invalid access cases")
			}
		}
	}
	if c.MetricsConfigFile != "" {
		collector, err := loadMetricCollector(c.MetricsConfigFile)
		if err != nil {
			return err
		}
		state.collector = collector
	}
	c.runtime = state
	return nil
}
func validateCaseClients(pool *tokenPool, cases *accessCases) error {
	ids := map[string]bool{"single": true}
	if pool != nil {
		ids = map[string]bool{}
		for _, client := range pool.clients {
			ids[client.ID] = true
		}
	}
	for _, c := range cases.Cases {
		for id := range c.ExpectedByClient {
			if !ids[id] {
				return fmt.Errorf("access cases: unknown client override")
			}
		}
	}
	if pool != nil {
		for _, client := range pool.clients {
			if client.IntentionallyInvalid {
				for _, c := range cases.Cases {
					if c.expectation(client.ID) != "invalid" {
						return fmt.Errorf("access cases: invalid credential expectation conflict")
					}
				}
			}
		}
	}
	return nil
}
