// Copyright 2026 The claude-code-model-gateway Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package modelcache

import (
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"

	"github.com/zchee/claude-code-model-gateway/internal/anthropiccatalog"
	"github.com/zchee/claude-code-model-gateway/internal/atomicfile"
	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/policy"
)

// discoveryModel is one gateway-models.json entry; the display name is omitted when the row has none.
type discoveryModel struct {
	ID          string  `json:"id"`
	DisplayName *string `json:"display_name,omitzero"`
}

// discoveryDocument is the whole gateway-models.json file (lib/runtime.js:629).
type discoveryDocument struct {
	BaseURL   string           `json:"baseUrl"`
	FetchedAt int64            `json:"fetchedAt"`
	Models    []discoveryModel `json:"models"`
}

// SyncDiscoveryCache writes the gateway discovery cache when EffectiveBaseURL returns DefaultBaseURL
// without error and Shadow is false. An error from EffectiveBaseURL is returned without writing.
func (c *Cache) SyncDiscoveryCache(force bool) (DiscoverySyncResult, error) {
	result := DiscoverySyncResult{CachePath: c.discoveryPath}
	if c.opts.Shadow {
		result.State, result.Reason = "skipped", "shadow"
		return result, nil
	}
	var baseURL string
	if c.opts.EffectiveBaseURL != nil {
		var err error
		if baseURL, err = c.opts.EffectiveBaseURL(); err != nil {
			return result, fmt.Errorf("modelcache: resolve the effective base URL: %w", err)
		}
	}
	// The retired compatibility URL is skipped like any other unwired URL: no write, delete or dial.
	if baseURL == "" || baseURL != c.opts.DefaultBaseURL {
		result.State, result.Reason = "skipped", "gateway-not-wired"
		return result, nil
	}
	if c.discoveryPath == "" {
		return result, errors.New("modelcache: the discovery cache path is unavailable")
	}

	// Discovery uses the committed snapshot, so its IDs match the picker rows already published.
	current := c.current.Load()
	cached, _ := catalogData(anthropiccatalog.Read(c.catalogPath()))
	rows := anthropiccatalog.WithPrevious(anthropiccatalog.WithCached(current.rows, cached, current.snapshot.Resolver()), c.serverRows(), c.currentPins())
	models, err := discoveryModels(rows, current.snapshot.Resolver())
	if err != nil {
		return result, err
	}

	c.discoveryMu.Lock()
	defer c.discoveryMu.Unlock()
	result.ModelCount = len(models)
	if existing, err := readSmallFile(c.discoveryPath); err == nil && !force && sameDiscoveryModels(existing, models) {
		result.State = "unchanged"
		return result, nil
	}
	data, err := jsonx.EncodeState(discoveryDocument{BaseURL: baseURL, FetchedAt: c.clock().UnixMilli(), Models: models})
	if err != nil {
		return result, err
	}
	if err := os.MkdirAll(filepath.Dir(c.discoveryPath), 0o777); err != nil {
		return result, err
	}
	if err := atomicfile.WriteFile(c.discoveryPath, data, 0o600); err != nil {
		return result, err
	}
	result.State = "wrote"
	return result, nil
}

// syncDiscovery runs a sync for refresh paths, which report a failure only to the log.
func (c *Cache) syncDiscovery(force bool) {
	if _, err := c.SyncDiscoveryCache(force); err != nil {
		c.logf("model-gateway: could not update discovery cache (%s)", errorClass(err))
	}
}

// discoveryModels keeps the rows whose ID names Claude or Anthropic and advertises each ID with the
// committed resolver (lib/runtime.js:589-597).
func discoveryModels(rows jsontext.Value, resolver policy.Resolver) ([]discoveryModel, error) {
	list, err := jsonx.DecodeWire(rows)
	if err != nil {
		return nil, err
	}
	models := make([]discoveryModel, 0, len(list.Elements))
	for _, row := range list.Elements {
		id := member(row, "id")
		if id.Kind != '"' {
			continue
		}
		text := string(utf16.Decode(id.Text))
		if lower := asciiLower(text); !strings.Contains(lower, "claude") && !strings.Contains(lower, "anthropic") {
			continue
		}
		entry := discoveryModel{ID: resolver.ClientID(text)}
		if name := member(row, "display_name"); name.Kind == '"' {
			entry.DisplayName = new(string(utf16.Decode(name.Text)))
		}
		models = append(models, entry)
	}
	return models, nil
}

// sameDiscoveryModels compares the cached models with the next ones by ID and display name
// (lib/runtime.js:603-608).
func sameDiscoveryModels(raw []byte, models []discoveryModel) bool {
	doc, err := jsonx.DecodeWire(raw)
	if err != nil {
		return false
	}
	list := member(doc, "models")
	if list.Kind != '[' || len(list.Elements) != len(models) {
		return false
	}
	for i, existing := range list.Elements {
		id := member(existing, "id")
		if id.Kind != '"' || string(utf16.Decode(id.Text)) != models[i].ID {
			return false
		}
		name := member(existing, "display_name")
		if models[i].DisplayName == nil {
			if name.Kind != 0 {
				return false
			}
			continue
		}
		if name.Kind != '"' || string(utf16.Decode(name.Text)) != *models[i].DisplayName {
			return false
		}
	}
	return true
}

// asciiLower folds ASCII letters only, as a JavaScript /i pattern without the u flag compares them.
func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, s)
}
