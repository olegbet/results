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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ktest "k8s.io/client-go/testing"
)

func TestUsersCleanupOwnership(t *testing.T) {
	k, opt := usersFixture(t)
	state, err := prepareUsers(context.Background(), k, "https://fixture", opt)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(opt.OutputDir, "ownership.json")
	unrelated := filepath.Join(opt.OutputDir, "keep.txt")
	_ = os.WriteFile(unrelated, []byte("keep"), 0600)
	if cleanupUsers(context.Background(), k, "https://other", opt.Context, path) == nil {
		t.Fatal("wrong endpoint accepted")
	}
	if cleanupUsers(context.Background(), k, "https://fixture", "wrong-context", path) == nil {
		t.Fatal("wrong context accepted")
	}
	// A replacement with the same preparation label must still survive.
	target := state.Objects[0]
	obj, err := k.CoreV1().ServiceAccounts(target.Namespace).Get(context.Background(), target.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	obj.UID = "replacement"
	_, _ = k.CoreV1().ServiceAccounts(target.Namespace).Update(context.Background(), obj, metav1.UpdateOptions{})
	if cleanupUsers(context.Background(), k, "https://fixture", opt.Context, path) == nil {
		t.Fatal("replacement accepted")
	}
	if _, err := k.CoreV1().ServiceAccounts(target.Namespace).Get(context.Background(), target.Name, metav1.GetOptions{}); err != nil {
		t.Fatal("replacement deleted")
	}
	obj.UID = target.UID
	_, _ = k.CoreV1().ServiceAccounts(target.Namespace).Update(context.Background(), obj, metav1.UpdateOptions{})
	k.PrependReactor("delete", "*", func(a ktest.Action) (bool, runtime.Object, error) {
		opts := a.(ktest.DeleteAction).GetDeleteOptions()
		if opts.Preconditions == nil || opts.Preconditions.UID == nil || *opts.Preconditions.UID == types.UID("") {
			t.Error("delete without UID precondition")
		}
		return false, nil, nil
	})
	for i := 0; i < 2; i++ {
		if err := cleanupUsers(context.Background(), k, "https://fixture", opt.Context, path); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatal("unrelated file deleted")
	}
	var receipt usersInventory
	if err := readStrictJSON(path, &receipt); err != nil || receipt.State != "cleaned" {
		t.Fatal("cleaned receipt missing")
	}
	if _, err := os.Stat(filepath.Join(opt.OutputDir, "pool.json")); !os.IsNotExist(err) {
		t.Fatal("credential manifest retained")
	}
}
func TestUsersLostCreateResponse(t *testing.T) {
	k, opt := usersFixture(t)
	first := true
	k.PrependReactor("create", "serviceaccounts", func(a ktest.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "" || !first {
			return false, nil, nil
		}
		first = false
		sa := a.(ktest.CreateAction).GetObject().(*corev1.ServiceAccount).DeepCopy()
		sa.UID = "lost-response"
		if err := k.Tracker().Create(corev1.SchemeGroupVersion.WithResource("serviceaccounts"), sa, sa.Namespace); err != nil {
			t.Fatal(err)
		}
		return true, nil, fmt.Errorf("lost response")
	})
	state, err := prepareUsers(context.Background(), k, "https://fixture", opt)
	if err == nil || state.State != "partial" {
		t.Fatal("lost create treated as complete")
	}
	if state.Objects[0].UID != "" {
		t.Fatal("test did not lose UID")
	}
	if err := cleanupUsers(context.Background(), k, "https://fixture", opt.Context, filepath.Join(opt.OutputDir, "ownership.json")); err != nil {
		t.Fatal(err)
	}
	accounts, _ := k.CoreV1().ServiceAccounts(opt.Namespace).List(context.Background(), metav1.ListOptions{})
	if len(accounts.Items) != 0 {
		t.Fatal("lost create not recovered")
	}
}

func TestUsersCleanupRejectsUnsafeState(t *testing.T) {
	for _, mode := range []string{"namespace", "file-symlink", "traversal", "directory-symlink", "foreign-marker"} {
		t.Run(mode, func(t *testing.T) {
			k, opt := usersFixture(t)
			state, err := prepareUsers(context.Background(), k, "https://fixture", opt)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(opt.OutputDir, "ownership.json")
			switch mode {
			case "namespace":
				ns, _ := k.CoreV1().Namespaces().Get(context.Background(), "access", metav1.GetOptions{})
				ns.UID = "replacement"
				_, _ = k.CoreV1().Namespaces().Update(context.Background(), ns, metav1.UpdateOptions{})
			case "file-symlink":
				_ = os.Remove(filepath.Join(opt.OutputDir, state.Clients[0].TokenFile))
				_ = os.Symlink(filepath.Join(t.TempDir(), "unowned"), filepath.Join(opt.OutputDir, state.Clients[0].TokenFile))
			case "traversal":
				state.Files[0] = "../unowned"
				root, _ := privateRoot(opt.OutputDir)
				if err := saveInventory(root, state); err != nil {
					t.Fatal(err)
				}
				_ = root.Close()
			case "directory-symlink":
				link := filepath.Join(t.TempDir(), "linked")
				_ = os.Symlink(opt.OutputDir, link)
				path = filepath.Join(link, "ownership.json")
			case "foreign-marker":
				o := state.Objects[0]
				sa, _ := k.CoreV1().ServiceAccounts(o.Namespace).Get(context.Background(), o.Name, metav1.GetOptions{})
				sa.Labels[preparationLabel] = "other"
				_, _ = k.CoreV1().ServiceAccounts(o.Namespace).Update(context.Background(), sa, metav1.UpdateOptions{})
			}
			if cleanupUsers(context.Background(), k, "https://fixture", opt.Context, path) == nil {
				t.Fatal("unsafe cleanup succeeded")
			}
			first := state.Objects[0]
			if _, err := k.CoreV1().ServiceAccounts(first.Namespace).Get(context.Background(), first.Name, metav1.GetOptions{}); err != nil {
				t.Fatal("unsafe target deleted")
			}
		})
	}
}
