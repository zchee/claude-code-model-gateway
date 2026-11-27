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
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf16"

	"github.com/zchee/claude-code-model-gateway/internal/anthropiccatalog"
	"github.com/zchee/claude-code-model-gateway/internal/atomicfile"
	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/policy"
)

// discoveryModel is one gateway-models.json entry. The display name keeps the row's exact UTF-16
// code units, an unpaired surrogate included; named is false when the row has no string name.
type discoveryModel struct {
	id    string
	name  []uint16
	named bool
}

// discoveryDocument is the whole gateway-models.json file in its member order (lib/runtime.js:629).
func discoveryDocument(baseURL string, fetchedAt int64, models []discoveryModel) jsonx.WireValue {
	list := jsonx.WireValue{Kind: '[', Elements: make([]jsonx.WireValue, 0, len(models))}
	for _, m := range models {
		entry := jsonx.WireValue{Kind: '{', Members: []jsonx.Member{{Name: utf16.Encode([]rune("id")), Value: wireText(m.id)}}}
		if m.named {
			entry.Members = append(entry.Members, jsonx.Member{Name: utf16.Encode([]rune("display_name")), Value: jsonx.WireValue{Kind: '"', Text: m.name}})
		}
		list.Elements = append(list.Elements, entry)
	}
	return jsonx.WireValue{Kind: '{', Members: []jsonx.Member{
		{Name: utf16.Encode([]rune("baseUrl")), Value: wireText(baseURL)},
		{Name: utf16.Encode([]rune("fetchedAt")), Value: jsonx.WireValue{Kind: '0', Number: float64(fetchedAt)}},
		{Name: utf16.Encode([]rune("models")), Value: list},
	}}
}

func wireText(s string) jsonx.WireValue {
	return jsonx.WireValue{Kind: '"', Text: utf16.Encode([]rune(s))}
}

// SyncDiscoveryCache writes the gateway discovery cache when EffectiveBaseURL returns DefaultBaseURL
// without error and Shadow is false. An error from EffectiveBaseURL is returned without writing.
func (c *Cache) SyncDiscoveryCache(force bool) (DiscoverySyncResult, error) {
	result := DiscoverySyncResult{CachePath: c.discoveryPath}
	if c.opts.Shadow {
		result.State, result.Reason = "skipped", "shadow"
		return result, nil
	}
	// The wiring, the committed generation, the Anthropic rows and the pins are read inside the same
	// serialized operation that writes the file, so a writer that read older inputs can never write
	// after one that read newer inputs. No commit lock is taken, so a sync completes while a refresh
	// waits on a local probe.
	c.discoveryMu.Lock()
	defer c.discoveryMu.Unlock()
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
	result.ModelCount = len(models)
	if existing, err := readSmallFile(c.discoveryPath); err == nil && !force && sameDiscoveryModels(existing, models) {
		result.State = "unchanged"
		return result, nil
	}
	data, err := jsonx.EncodeState(discoveryDocument(baseURL, c.clock().UnixMilli(), models))
	if err != nil {
		return result, err
	}
	// A missing directory has no watcher yet, so this write creates it with its staging
	// subdirectory; an existing one got that subdirectory when the cache was created.
	if dir := filepath.Dir(c.discoveryPath); !exists(dir) {
		if err := atomicfile.CreateStagingDir(dir, 0o777); err != nil {
			return result, err
		}
	}
	// Like the Anthropic catalog, the discovery cache is rebuilt from the catalogs, so it is not
	// synced, and a reader may watch its directory, so it is staged out of that view and renamed in.
	if err := atomicfile.WriteFile(c.discoveryPath, data, 0o600, atomicfile.SkipSync(), atomicfile.PrivateStaging()); err != nil {
		return result, err
	}
	result.State = "wrote"
	return result, nil
}

// exists reports whether path names an existing file system entry, following symlinks.
func exists(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, fs.ErrNotExist)
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
		entry := discoveryModel{id: resolver.ClientID(text)}
		if name := member(row, "display_name"); name.Kind == '"' {
			entry.name, entry.named = slices.Clone(name.Text), true
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
		if id.Kind != '"' || string(utf16.Decode(id.Text)) != models[i].id {
			return false
		}
		name := member(existing, "display_name")
		if !models[i].named {
			if name.Kind != 0 {
				return false
			}
			continue
		}
		if name.Kind != '"' || !slices.Equal(name.Text, models[i].name) {
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
