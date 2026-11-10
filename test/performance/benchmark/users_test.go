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
	"fmt"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	ktest "k8s.io/client-go/testing"
)

func TestUsersCommandsProfilesAndFlags(t *testing.T) {
	for _, profile := range []string{"read", "read-write", "none"} {
		k, opt := usersFixture(t)
		root := &cobra.Command{Use: "bench"}
		root.AddCommand(newUsersCmdWithClient(func(contextName, kubeconfig string) (kubernetes.Interface, string, error) {
			if contextName != "explicit" || kubeconfig != "" {
				t.Fatal("context substitution")
			}
			return k, "https://fixture", nil
		}))
		flags := []string{"users", "prepare", "--context", opt.Context, "--namespace", opt.Namespace, "--profile", profile, "--count", "3", "--token-duration", "24h", "--output-dir", opt.OutputDir}
		if profile != "none" {
			flags = append(flags, "--access-namespaces", "access")
		}
		var summary bytes.Buffer
		root.SetOut(&summary)
		root.SetArgs(flags)
		if err := root.ExecuteContext(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := loadTokenPool(filepath.Join(opt.OutputDir, "pool.json"), 0); err != nil {
			t.Fatal(err)
		}
		roles, _ := k.RbacV1().Roles("access").List(context.Background(), metav1.ListOptions{})
		if profile == "none" && len(roles.Items) != 0 {
			t.Fatal("none profile granted access")
		}
		if profile == "read-write" && fmt.Sprint(roles.Items[0].Rules[0].Verbs) != "[get list create update]" {
			t.Fatal("wrong write grants")
		}
	}
	for _, flags := range [][]string{{"users", "prepare"}, {"users", "cleanup", "--context", "explicit", "--state", "private", "--metrics-config", "private"}, {"users", "prepare", "--context", "explicit", "--namespace", "identities", "--profile", "none", "--count", "1", "--token-duration", "1h", "--output-dir", "private", "--access-namespaces", "access"}} {
		called := false
		root := &cobra.Command{Use: "bench"}
		root.AddCommand(newUsersCmdWithClient(func(string, string) (kubernetes.Interface, string, error) {
			called = true
			return nil, "", fmt.Errorf("unexpected")
		}))
		root.SilenceErrors = true
		root.SilenceUsage = true
		root.SetArgs(flags)
		if root.Execute() == nil || called {
			t.Fatal("invalid flags contacted Kubernetes")
		}
	}
}
func TestUsersInventoryPrecedesMutationAndCancellation(t *testing.T) {
	k, opt := usersFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	k.PrependReactor("create", "*", func(a ktest.Action) (bool, runtime.Object, error) {
		var state usersInventory
		if err := readStrictJSON(filepath.Join(opt.OutputDir, "ownership.json"), &state); err != nil || len(state.Objects) == 0 || state.State != "preparing" {
			t.Fatal("mutation before durable intent")
		}
		if a.GetSubresource() == "token" {
			cancel()
		}
		return false, nil, nil
	})
	state, err := prepareUsers(ctx, k, "https://fixture", opt)
	if err == nil || state.State != "partial" {
		t.Fatal("interrupted preparation published ready pool")
	}
	if err := cleanupUsers(context.Background(), k, "https://fixture", opt.Context, filepath.Join(opt.OutputDir, "ownership.json")); err != nil {
		t.Fatal(err)
	}
}
