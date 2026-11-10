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
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const preparationLabel = "benchmark.results.tekton.dev/preparation"

type usersOptions struct {
	Context, Kubeconfig, Namespace, Profile, OutputDir string
	AccessNamespaces                                   []string
	Count                                              int
	TokenDuration                                      time.Duration
}
type ownedObject struct {
	Kind      string    `json:"kind"`
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	UID       types.UID `json:"uid,omitempty"`
}
type usersInventory struct {
	SchemaVersion string               `json:"schema_version"`
	PreparationID string               `json:"preparation_id"`
	State         string               `json:"state"`
	Context       string               `json:"context"`
	Endpoint      string               `json:"endpoint"`
	Namespace     string               `json:"namespace"`
	NamespaceUIDs map[string]types.UID `json:"namespace_uids"`
	Requested     int                  `json:"requested"`
	Objects       []ownedObject        `json:"objects"`
	Clients       []poolEntry          `json:"clients"`
	Files         []string             `json:"files"`
}

// privateRoot rejects symlinks in the directory and all its ancestors. Root
// operations additionally confine every access to this directory.
func privateRoot(dir string) (*os.Root, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("invalid-private-directory")
	}
	for current := abs; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("unsafe-private-directory")
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	info, err := os.Stat(abs)
	if err != nil || info.Mode().Perm() != 0700 {
		return nil, fmt.Errorf("private-directory-mode")
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, fmt.Errorf("private-directory-unreadable")
	}
	return root, nil
}
func safeOwnedPath(root *os.Root, path string) error {
	if !filepath.IsLocal(path) || filepath.Clean(path) != path {
		return fmt.Errorf("unsafe-owned-path")
	}
	parts := strings.Split(path, string(filepath.Separator))
	for i := range parts {
		info, err := root.Lstat(filepath.Join(parts[:i+1]...))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe-owned-path")
		}
		if i < len(parts)-1 && !info.IsDir() {
			return fmt.Errorf("unsafe-owned-path")
		}
	}
	return nil
}
func writePrivate(root *os.Root, path string, data []byte, replace bool) error {
	if err := safeOwnedPath(root, path); err != nil {
		return err
	}
	target := path
	if replace {
		target = ".inventory-" + uuid.NewString()
	}
	f, err := root.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("private-write-failed")
	}
	if replace {
		defer func() { _ = root.Remove(target) }()
	}
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return fmt.Errorf("private-write-failed")
	}
	if replace {
		if err := root.Rename(target, path); err != nil {
			return fmt.Errorf("inventory-persist-failed")
		}
	}
	directory, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("private-directory-sync-failed")
	}
	defer func() { _ = directory.Close() }()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("private-directory-sync-failed")
	}
	return nil
}
func saveInventory(root *os.Root, state *usersInventory) error {
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("inventory-encoding-failed")
	}
	return writePrivate(root, "ownership.json", append(b, '\n'), true)
}
func validateUsersOptions(o usersOptions) error {
	if o.Context == "" || !namespaceName.MatchString(o.Namespace) || o.OutputDir == "" || o.Count < 1 || o.TokenDuration < time.Second {
		return fmt.Errorf("invalid-preparation-inputs")
	}
	if o.Profile != "read" && o.Profile != "read-write" && o.Profile != "none" {
		return fmt.Errorf("invalid-access-profile")
	}
	if (o.Profile == "none") != (len(o.AccessNamespaces) == 0) {
		return fmt.Errorf("access-namespace-profile-conflict")
	}
	seen := map[string]bool{}
	for _, ns := range o.AccessNamespaces {
		if !namespaceName.MatchString(ns) || seen[ns] {
			return fmt.Errorf("invalid-access-namespaces")
		}
		seen[ns] = true
	}
	return nil
}
func prepareUsers(ctx context.Context, k kubernetes.Interface, endpoint string, o usersOptions) (state *usersInventory, err error) {
	if err := validateUsersOptions(o); err != nil {
		return nil, err
	}
	namespaces := map[string]types.UID{}
	for _, name := range append([]string{o.Namespace}, o.AccessNamespaces...) {
		ns, err := k.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
		if err != nil || ns.UID == "" {
			return nil, fmt.Errorf("namespace-verification-failed")
		}
		namespaces[name] = ns.UID
	}
	// Inspect the parent before creating the new private directory.
	parent, err := filepath.Abs(filepath.Dir(o.OutputDir))
	if err != nil {
		return nil, fmt.Errorf("invalid-output-directory")
	}
	for current := parent; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("unsafe-output-parent")
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	if err := os.Mkdir(o.OutputDir, 0700); err != nil {
		return nil, fmt.Errorf("output-directory-must-be-new")
	}
	root, err := privateRoot(o.OutputDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	if err := root.Mkdir("tokens", 0700); err != nil {
		return nil, fmt.Errorf("token-directory-failed")
	}
	id := uuid.NewString()
	state = &usersInventory{SchemaVersion: "1", PreparationID: id, State: "preparing", Context: o.Context, Endpoint: endpoint, Namespace: o.Namespace, NamespaceUIDs: namespaces, Requested: o.Count, Files: []string{"pool.json"}}
	for i := 0; i < o.Count; i++ {
		clientID := fmt.Sprintf("user-%04d", i)
		name := "bench-" + id + fmt.Sprintf("-%d", i)
		state.Objects = append(state.Objects, ownedObject{Kind: "ServiceAccount", Namespace: o.Namespace, Name: name})
		state.Clients = append(state.Clients, poolEntry{ID: clientID, TokenFile: "tokens/" + clientID, Identity: &poolIdentity{Username: "system:serviceaccount:" + o.Namespace + ":" + name, Provenance: "provisioned", EvidenceRef: "ownership.json"}})
		state.Files = append(state.Files, "tokens/"+clientID)
	}
	for _, ns := range o.AccessNamespaces {
		for _, kind := range []string{"Role", "RoleBinding"} {
			state.Objects = append(state.Objects, ownedObject{Kind: kind, Namespace: ns, Name: "bench-" + id})
		}
	}
	if err := saveInventory(root, state); err != nil {
		return state, err
	}
	defer func() {
		if err != nil {
			state.State = "partial"
			if persistErr := saveInventory(root, state); persistErr != nil {
				err = fmt.Errorf("preparation-failed; partial-inventory-persist-failed")
			}
		}
	}()
	labels := map[string]string{preparationLabel: id}
	subjects := make([]rbacv1.Subject, 0, o.Count)
	for i := 0; i < o.Count; i++ {
		subjects = append(subjects, rbacv1.Subject{Kind: "ServiceAccount", Name: state.Objects[i].Name, Namespace: o.Namespace})
	}
	verbs := []string{"get", "list"}
	if o.Profile == "read-write" {
		verbs = append(verbs, "create", "update")
	}
	for i := range state.Objects {
		if ctx.Err() != nil {
			return state, fmt.Errorf("preparation-interrupted")
		}
		object := &state.Objects[i]
		meta := metav1.ObjectMeta{Name: object.Name, Namespace: object.Namespace, Labels: labels}
		var created metav1.Object
		switch object.Kind {
		case "ServiceAccount":
			created, err = k.CoreV1().ServiceAccounts(object.Namespace).Create(ctx, &corev1.ServiceAccount{ObjectMeta: meta}, metav1.CreateOptions{})
		case "Role":
			created, err = k.RbacV1().Roles(object.Namespace).Create(ctx, &rbacv1.Role{ObjectMeta: meta, Rules: []rbacv1.PolicyRule{{APIGroups: []string{"results.tekton.dev"}, Resources: []string{"results", "records"}, Verbs: verbs}}}, metav1.CreateOptions{})
		case "RoleBinding":
			created, err = k.RbacV1().RoleBindings(object.Namespace).Create(ctx, &rbacv1.RoleBinding{ObjectMeta: meta, Subjects: subjects, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: object.Name}}, metav1.CreateOptions{})
		}
		if err != nil {
			return state, fmt.Errorf("preparation-object-create-failed")
		}
		object.UID = created.GetUID()
		if object.UID == "" {
			return state, fmt.Errorf("preparation-object-uid-missing")
		}
		if object.Kind == "ServiceAccount" {
			state.Clients[i].Identity.UID = string(object.UID)
		}
		if err := saveInventory(root, state); err != nil {
			return state, err
		}
	}
	seconds := int64(o.TokenDuration / time.Second)
	issued := map[string]bool{}
	for i := range state.Clients {
		if ctx.Err() != nil {
			return state, fmt.Errorf("preparation-interrupted")
		}
		entry := &state.Clients[i]
		response, e := k.CoreV1().ServiceAccounts(o.Namespace).CreateToken(ctx, state.Objects[i].Name, &authv1.TokenRequest{Spec: authv1.TokenRequestSpec{ExpirationSeconds: &seconds}}, metav1.CreateOptions{})
		if e != nil || response == nil || response.Status.Token == "" || !response.Status.ExpirationTimestamp.After(time.Now()) {
			return state, fmt.Errorf("preparation-token-issuance-failed")
		}
		token := response.Status.Token
		if strings.TrimSpace(token) != token || strings.ContainsFunc(token, unicode.IsSpace) || issued[token] {
			return state, fmt.Errorf("preparation-invalid-token-response")
		}
		issued[token] = true
		expires := response.Status.ExpirationTimestamp.Time
		entry.ExpiresAt = &expires
		if err := writePrivate(root, entry.TokenFile, []byte(response.Status.Token), false); err != nil {
			return state, err
		}
		if err := saveInventory(root, state); err != nil {
			return state, err
		}
	}
	if ctx.Err() != nil {
		return state, fmt.Errorf("preparation-interrupted")
	}
	manifest := poolManifest{SchemaVersion: "1", PopulationID: id, Clients: state.Clients}
	b, e := json.MarshalIndent(manifest, "", "  ")
	if e != nil {
		return state, fmt.Errorf("pool-encoding-failed")
	}
	if err := writePrivate(root, "pool.json", append(b, '\n'), true); err != nil {
		return state, err
	}
	state.State = "ready"
	if err := saveInventory(root, state); err != nil {
		_ = root.Remove("pool.json")
		return state, err
	}
	return state, nil
}

func validateInventory(s *usersInventory) error {
	if s.SchemaVersion != "1" || !publicID.MatchString(s.PreparationID) || s.Context == "" || s.Endpoint == "" || !namespaceName.MatchString(s.Namespace) || s.NamespaceUIDs[s.Namespace] == "" || s.Requested < 1 || len(s.Clients) != s.Requested {
		return fmt.Errorf("invalid-ownership-inventory")
	}
	switch s.State {
	case "preparing", "partial", "ready", "cleaning", "cleaned":
	default:
		return fmt.Errorf("invalid-inventory-state")
	}
	allowedFiles := map[string]bool{"pool.json": true}
	for i, c := range s.Clients {
		if c.ID != fmt.Sprintf("user-%04d", i) || c.TokenFile != "tokens/"+c.ID {
			return fmt.Errorf("invalid-owned-client")
		}
		allowedFiles[c.TokenFile] = true
	}
	if len(s.Files) != len(allowedFiles) {
		return fmt.Errorf("invalid-owned-files")
	}
	for _, file := range s.Files {
		if !allowedFiles[file] {
			return fmt.Errorf("invalid-owned-file")
		}
		delete(allowedFiles, file)
	}
	seen := map[string]bool{}
	for i, o := range s.Objects {
		if s.NamespaceUIDs[o.Namespace] == "" || !namespaceName.MatchString(o.Namespace) {
			return fmt.Errorf("invalid-owned-namespace")
		}
		expected := "bench-" + s.PreparationID
		if o.Kind == "ServiceAccount" {
			expected += fmt.Sprintf("-%d", i)
			if i >= s.Requested || o.Namespace != s.Namespace {
				return fmt.Errorf("invalid-owned-account")
			}
		} else if (o.Kind != "Role" && o.Kind != "RoleBinding") || i < s.Requested {
			return fmt.Errorf("invalid-owned-kind")
		}
		key := o.Kind + "/" + o.Namespace + "/" + o.Name
		if o.Name != expected || seen[key] {
			return fmt.Errorf("invalid-owned-object")
		}
		seen[key] = true
	}
	if len(s.Objects) < s.Requested {
		return fmt.Errorf("missing-owned-objects")
	}
	return nil
}
func cleanupUsers(ctx context.Context, k kubernetes.Interface, endpoint, contextName, path string) error {
	if filepath.Base(path) != "ownership.json" {
		return fmt.Errorf("invalid-inventory-path")
	}
	root, err := privateRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := safeOwnedPath(root, "ownership.json"); err != nil {
		return err
	}
	var state usersInventory
	b, err := root.ReadFile("ownership.json")
	if err != nil {
		return fmt.Errorf("inventory-unreadable")
	}
	// Strict parsing through a root-backed file avoids following a substituted path.
	if err := decodeInventory(b, &state); err != nil {
		return err
	}
	if err := validateInventory(&state); err != nil {
		return err
	}
	if state.Context != contextName || state.Endpoint != endpoint {
		return fmt.Errorf("cleanup-target-mismatch")
	}
	for name, uid := range state.NamespaceUIDs {
		ns, err := k.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
		if err != nil || uid == "" || ns.UID != uid {
			return fmt.Errorf("cleanup-namespace-mismatch")
		}
	}
	if state.State == "cleaned" {
		return nil
	}
	for _, file := range state.Files {
		if err := safeOwnedPath(root, file); err != nil {
			return err
		}
	}
	state.State = "cleaning"
	if err := saveInventory(root, &state); err != nil {
		return err
	}
	failed := false
	// Bindings/grants are removed before accounts. Continue independent deletions
	// on conflicts, preserving a partial receipt for a later safe retry.
	for i := len(state.Objects) - 1; i >= 0; i-- {
		o := &state.Objects[i]
		obj, err := getOwnedObject(ctx, k, *o)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil || obj.GetLabels()[preparationLabel] != state.PreparationID || obj.GetUID() == "" || (o.UID != "" && o.UID != obj.GetUID()) {
			failed = true
			continue
		}
		if o.UID == "" {
			o.UID = obj.GetUID()
			if err := saveInventory(root, &state); err != nil {
				return err
			}
		}
		opts := metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &o.UID}}
		err = deleteOwnedObject(ctx, k, *o, opts)
		if err != nil && !apierrors.IsNotFound(err) {
			failed = true
		}
	}
	if !failed {
		for _, file := range state.Files {
			if err := root.Remove(file); err != nil && !os.IsNotExist(err) {
				failed = true
			}
		}
		// Empty-only removal preserves any unrelated local files.
		if err := root.Remove("tokens"); err != nil && !os.IsNotExist(err) {
			// A nonempty token directory is retained; owned files were removed individually.
			if info, e := root.Stat("tokens"); e != nil || !info.IsDir() {
				failed = true
			}
		}
	}
	state.State = "cleaned"
	if failed {
		state.State = "partial"
	}
	if err := saveInventory(root, &state); err != nil {
		return err
	}
	if failed {
		return fmt.Errorf("cleanup-partial: ownership-or-deletion-failed")
	}
	return nil
}
func decodeInventory(b []byte, dst *usersInventory) error {
	// Use the same strict schema/duplicate-key policy as all benchmark inputs.
	if err := uniqueJSON(json.NewDecoder(strings.NewReader(string(b)))); err != nil {
		return fmt.Errorf("invalid-inventory-json")
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid-inventory-json")
	}
	if dec.Decode(new(any)) != io.EOF {
		return fmt.Errorf("invalid-inventory-json")
	}
	return nil
}
func getOwnedObject(ctx context.Context, k kubernetes.Interface, o ownedObject) (metav1.Object, error) {
	switch o.Kind {
	case "ServiceAccount":
		return k.CoreV1().ServiceAccounts(o.Namespace).Get(ctx, o.Name, metav1.GetOptions{})
	case "Role":
		return k.RbacV1().Roles(o.Namespace).Get(ctx, o.Name, metav1.GetOptions{})
	case "RoleBinding":
		return k.RbacV1().RoleBindings(o.Namespace).Get(ctx, o.Name, metav1.GetOptions{})
	default:
		return nil, fmt.Errorf("invalid-owned-kind")
	}
}
func deleteOwnedObject(ctx context.Context, k kubernetes.Interface, o ownedObject, opts metav1.DeleteOptions) error {
	switch o.Kind {
	case "ServiceAccount":
		return k.CoreV1().ServiceAccounts(o.Namespace).Delete(ctx, o.Name, opts)
	case "Role":
		return k.RbacV1().Roles(o.Namespace).Delete(ctx, o.Name, opts)
	case "RoleBinding":
		return k.RbacV1().RoleBindings(o.Namespace).Delete(ctx, o.Name, opts)
	default:
		return fmt.Errorf("invalid-owned-kind")
	}
}
func loadUsersClient(contextName, kubeconfig string) (kubernetes.Interface, string, error) {
	if contextName == "" {
		return nil, "", fmt.Errorf("explicit-context-required")
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: contextName}).ClientConfig()
	if err != nil {
		return nil, "", fmt.Errorf("kubernetes-context-unavailable")
	}
	cfg.Timeout = 30 * time.Second
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, "", fmt.Errorf("kubernetes-client-unavailable")
	}
	return client, cfg.Host, nil
}
func newUsersCmd() *cobra.Command { return newUsersCmdWithClient(loadUsersClient) }
func newUsersCmdWithClient(factory func(string, string) (kubernetes.Interface, string, error)) *cobra.Command {
	users := &cobra.Command{Use: "users", Short: "Prepare and clean up a private test-user population"}
	o := usersOptions{}
	prepare := &cobra.Command{Use: "prepare", Args: cobra.NoArgs, Short: "Prepare distinct service accounts and short-lived tokens", RunE: func(cmd *cobra.Command, _ []string) error {
		if err := validateUsersOptions(o); err != nil {
			return err
		}
		if o.Profile == "none" && cmd.Flags().Changed("access-namespaces") {
			return fmt.Errorf("none-profile-requires-omitted-access-namespaces")
		}
		k, endpoint, err := factory(o.Context, o.Kubeconfig)
		if err != nil {
			return err
		}
		state, err := prepareUsers(cmd.Context(), k, endpoint, o)
		if state != nil {
			created := 0
			var earliest *time.Time
			for _, c := range state.Clients {
				if c.Identity.UID != "" {
					created++
				}
				if c.ExpiresAt != nil && (earliest == nil || c.ExpiresAt.Before(*earliest)) {
					earliest = c.ExpiresAt
				}
			}
			summary := struct {
				PreparationID  string     `json:"preparation_id"`
				Requested      int        `json:"requested"`
				Created        int        `json:"created"`
				State          string     `json:"state"`
				PoolPath       string     `json:"pool_path,omitempty"`
				InventoryPath  string     `json:"inventory_path"`
				EarliestExpiry *time.Time `json:"earliest_expiry"`
			}{PreparationID: state.PreparationID, Requested: state.Requested, Created: created, State: state.State, InventoryPath: filepath.Join(o.OutputDir, "ownership.json"), EarliestExpiry: earliest}
			if state.State == "ready" {
				summary.PoolPath = filepath.Join(o.OutputDir, "pool.json")
			}
			if outputErr := json.NewEncoder(cmd.OutOrStdout()).Encode(summary); outputErr != nil && err == nil {
				return fmt.Errorf("preparation-summary-write-failed")
			}
		}
		return err
	}}
	f := prepare.Flags()
	f.StringVar(&o.Context, "context", "", "explicit kubeconfig context")
	f.StringVar(&o.Kubeconfig, "kubeconfig", "", "optional kubeconfig file")
	f.StringVar(&o.Namespace, "namespace", "", "existing identity namespace")
	f.StringSliceVar(&o.AccessNamespaces, "access-namespaces", nil, "existing access namespaces")
	f.StringVar(&o.Profile, "profile", "", "read|read-write|none")
	f.IntVar(&o.Count, "count", 0, "distinct users to prepare")
	f.DurationVar(&o.TokenDuration, "token-duration", 0, "requested token lifetime")
	f.StringVar(&o.OutputDir, "output-dir", "", "new private output directory")
	for _, flag := range []string{"context", "namespace", "profile", "count", "token-duration", "output-dir"} {
		_ = prepare.MarkFlagRequired(flag)
	}
	var contextName, kubeconfig, statePath string
	cleanup := &cobra.Command{Use: "cleanup", Args: cobra.NoArgs, Short: "Delete only inventoried resources and credentials", RunE: func(cmd *cobra.Command, _ []string) error {
		k, endpoint, err := factory(contextName, kubeconfig)
		if err != nil {
			return err
		}
		if err := cleanupUsers(cmd.Context(), k, endpoint, contextName, statePath); err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), `{"state":"cleaned"}`)
		if err != nil {
			return fmt.Errorf("cleanup-summary-write-failed")
		}
		return nil
	}}
	cleanup.Flags().StringVar(&contextName, "context", "", "explicit kubeconfig context")
	cleanup.Flags().StringVar(&kubeconfig, "kubeconfig", "", "optional kubeconfig file")
	cleanup.Flags().StringVar(&statePath, "state", "", "owned ownership.json path")
	_ = cleanup.MarkFlagRequired("context")
	_ = cleanup.MarkFlagRequired("state")
	users.AddCommand(prepare, cleanup)
	return users
}
