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
	"path/filepath"
	"testing"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktest "k8s.io/client-go/testing"
)

func usersFixture(t *testing.T) (*fake.Clientset, usersOptions) {
	t.Helper()
	k := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "identities", UID: "identity-ns"}}, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "access", UID: "access-ns"}})
	sequence := 0
	k.PrependReactor("create", "*", func(a ktest.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() == "token" {
			sequence++
			expiry := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
			return true, &authv1.TokenRequest{Status: authv1.TokenRequestStatus{Token: fmt.Sprintf("synthetic-issued-%d", sequence), ExpirationTimestamp: metav1.NewTime(expiry)}}, nil
		}
		obj := a.(ktest.CreateAction).GetObject()
		m := obj.(metav1.Object)
		sequence++
		m.SetUID(types.UID(fmt.Sprintf("object-%d", sequence)))
		return false, nil, nil
	})
	return k, usersOptions{Context: "explicit", Namespace: "identities", AccessNamespaces: []string{"access"}, Profile: "read", Count: 3, TokenDuration: 24 * time.Hour, OutputDir: filepath.Join(t.TempDir(), "population")}
}
func TestUsersPreparePopulation(t *testing.T) {
	for _, count := range []int{3, 1000} {
		k, opt := usersFixture(t)
		opt.Count = count
		inventory, err := prepareUsers(context.Background(), k, "https://fixture", opt)
		if err != nil {
			t.Fatal(err)
		}
		if inventory.State != "ready" || len(inventory.Clients) != count {
			t.Fatal("population incomplete")
		}
		pool, err := loadTokenPool(filepath.Join(opt.OutputDir, "pool.json"), 0)
		if err != nil {
			t.Fatal(err)
		}
		if pool.summary.ProvisionedUsers != count || pool.summary.SupportedDistinctUsers != count {
			t.Fatal("identities not distinct")
		}
		if !pool.clients[0].ExpiresAt.Before(time.Now().Add(3 * time.Hour)) {
			t.Fatal("used requested rather than returned expiry")
		}
		for _, path := range []string{opt.OutputDir, filepath.Join(opt.OutputDir, "tokens"), filepath.Join(opt.OutputDir, "ownership.json"), filepath.Join(opt.OutputDir, "pool.json"), filepath.Join(opt.OutputDir, pool.clients[0].TokenFile)} {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			want := os.FileMode(0600)
			if info.IsDir() {
				want = 0700
			}
			if info.Mode().Perm() != want {
				t.Fatal("public preparation artifact")
			}
		}
		roles, err := k.RbacV1().Roles("access").List(context.Background(), metav1.ListOptions{})
		if err != nil || len(roles.Items) != 1 {
			t.Fatal("role missing")
		}
		if fmt.Sprint(roles.Items[0].Rules[0].Verbs) != "[get list]" {
			t.Fatal("overbroad profile")
		}
		for _, a := range k.Actions() {
			if a.GetNamespace() == "" && a.GetVerb() != "get" {
				t.Fatal("cluster-wide mutation")
			}
		}
	}
}
func TestUsersPreparationPartial(t *testing.T) {
	k, opt := usersFixture(t)
	k.PrependReactor("create", "serviceaccounts", func(a ktest.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() == "token" {
			return true, nil, fmt.Errorf("synthetic-private-token-detail")
		}
		return false, nil, nil
	})
	state, err := prepareUsers(context.Background(), k, "https://fixture", opt)
	if err == nil || state.State != "partial" {
		t.Fatal("partial preparation called ready")
	}
	if _, err := os.Stat(filepath.Join(opt.OutputDir, "pool.json")); !os.IsNotExist(err) {
		t.Fatal("partial pool published")
	}
	if err := cleanupUsers(context.Background(), k, "https://fixture", opt.Context, filepath.Join(opt.OutputDir, "ownership.json")); err != nil {
		t.Fatal(err)
	}
	accounts, _ := k.CoreV1().ServiceAccounts(opt.Namespace).List(context.Background(), metav1.ListOptions{})
	if len(accounts.Items) != 0 {
		t.Fatal("partial objects leaked")
	}
}
