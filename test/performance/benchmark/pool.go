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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/tektoncd/results/test/performance/report"
)

var publicID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

type poolIdentity struct {
	Username    string `json:"username"`
	UID         string `json:"uid,omitempty"`
	Provenance  string `json:"provenance"`
	EvidenceRef string `json:"evidence_ref,omitempty"`
}
type poolEntry struct {
	ID                   string        `json:"id"`
	TokenFile            string        `json:"token_file"`
	Identity             *poolIdentity `json:"identity"`
	ExpiresAt            *time.Time    `json:"expires_at,omitempty"`
	IntentionallyInvalid bool          `json:"intentionally_invalid,omitempty"`
}
type poolManifest struct {
	SchemaVersion string      `json:"schema_version"`
	PopulationID  string      `json:"population_id"`
	Clients       []poolEntry `json:"clients"`
}
type poolClient struct {
	poolEntry
	token string
}
type tokenPool struct {
	clients []poolClient
	summary report.ClientPool
}

// readStrictJSON never returns parser text, which may contain a private value.
func readStrictJSON(path string, dst any) error {
	b, err := os.ReadFile(path) //nolint:gosec // explicit operator-supplied input
	if err != nil {
		return fmt.Errorf("input-unreadable")
	}
	if err := uniqueJSON(json.NewDecoder(bytes.NewReader(b))); err != nil {
		return fmt.Errorf("invalid-json")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid-json")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("trailing-json")
	}
	return nil
}

func uniqueJSON(d *json.Decoder) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return fmt.Errorf("duplicate-key")
			}
			seen[name] = true
			if err := uniqueJSON(d); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := uniqueJSON(d); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("invalid-delimiter")
	}
	_, err = d.Token()
	return err
}

func loadTokenPool(path string, duration time.Duration) (*tokenPool, error) {
	var manifest poolManifest
	if err := readStrictJSON(path, &manifest); err != nil {
		return nil, fmt.Errorf("token pool: %w", err)
	}
	if manifest.SchemaVersion != "1" || !publicID.MatchString(manifest.PopulationID) || len(manifest.Clients) == 0 {
		return nil, fmt.Errorf("token pool: invalid-header")
	}
	p := &tokenPool{summary: report.ClientPool{PopulationID: manifest.PopulationID, ConfiguredClients: len(manifest.Clients), Selection: "round-robin-logical-operation"}}
	ids, tokens := map[string]bool{}, map[string]bool{}
	type fingerprintEntry struct {
		ID          string
		Identity    *poolIdentity
		Invalid     bool
		TokenDigest string
	}
	canonical := make([]fingerprintEntry, 0, len(manifest.Clients))
	for i, entry := range manifest.Clients {
		invalid := func(reason string) (*tokenPool, error) { return nil, fmt.Errorf("token pool entry %d: %s", i, reason) }
		if !publicID.MatchString(entry.ID) || ids[entry.ID] {
			return invalid("invalid-or-duplicate-id")
		}
		ids[entry.ID] = true
		if entry.TokenFile == "" {
			return invalid("missing-token-file")
		}
		tokenPath := entry.TokenFile
		if !filepath.IsAbs(tokenPath) {
			tokenPath = filepath.Join(filepath.Dir(path), tokenPath)
		}
		b, err := os.ReadFile(tokenPath) //nolint:gosec // explicit private token reference
		if err != nil {
			return invalid("token-unreadable")
		}
		token := strings.TrimSpace(string(b))
		if token == "" || strings.ContainsFunc(token, unicode.IsSpace) || tokens[token] {
			return invalid("invalid-or-duplicate-token")
		}
		tokens[token] = true
		if entry.IntentionallyInvalid {
			if entry.Identity != nil {
				return invalid("invalid-identity")
			}
		} else {
			id := entry.Identity
			if id == nil || strings.TrimSpace(id.Username) == "" {
				return invalid("missing-identity")
			}
			switch id.Provenance {
			case "declared":
			case "verified", "provisioned":
				if id.EvidenceRef == "" {
					return invalid("missing-evidence")
				}
			default:
				return invalid("invalid-provenance")
			}
			if id.Provenance == "provisioned" && id.UID == "" {
				return invalid("missing-uid")
			}
			if entry.ExpiresAt != nil && !entry.ExpiresAt.After(time.Now().Add(duration)) {
				return invalid("credential-expiry")
			}
		}
		p.clients = append(p.clients, poolClient{poolEntry: entry, token: token})
		var identity *poolIdentity
		if entry.Identity != nil {
			cloned := *entry.Identity
			cloned.EvidenceRef = ""
			identity = &cloned
		}
		digest := sha256.Sum256([]byte(token))
		canonical = append(canonical, fingerprintEntry{entry.ID, identity, entry.IntentionallyInvalid, hex.EncodeToString(digest[:])})
	}
	data, err := json.Marshal(canonical)
	if err != nil {
		return nil, fmt.Errorf("token pool: fingerprint-failed")
	}
	digest := sha256.Sum256(data)
	p.summary.PoolFingerprint = hex.EncodeToString(digest[:])
	p.countUsers()
	return p, nil
}

func (p *tokenPool) countUsers() {
	users := map[string][]*poolIdentity{}
	uidUsers := map[string]map[string]bool{}
	for _, c := range p.clients {
		id := c.Identity
		if id == nil {
			continue
		}
		users[id.Username] = append(users[id.Username], id)
		if id.UID != "" {
			if uidUsers[id.UID] == nil {
				uidUsers[id.UID] = map[string]bool{}
			}
			uidUsers[id.UID][id.Username] = true
		}
	}
	p.summary.DistinctUsers = len(users)
	for _, identities := range users {
		declared, provisioned := false, false
		uids := map[string]bool{}
		for _, id := range identities {
			declared = declared || id.Provenance == "declared"
			provisioned = provisioned || id.Provenance == "provisioned"
			if id.UID != "" {
				uids[id.UID] = true
				declared = declared || len(uidUsers[id.UID]) > 1
			}
		}
		declared = declared || len(uids) > 1
		switch {
		case declared:
			p.summary.DeclaredUsers++
		case provisioned:
			p.summary.ProvisionedUsers++
		default:
			p.summary.VerifiedUsers++
		}
	}
	p.summary.SupportedDistinctUsers = p.summary.ProvisionedUsers + p.summary.VerifiedUsers
}
