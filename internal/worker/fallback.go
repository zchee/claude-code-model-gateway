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

package worker

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"strings"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
)

// defaultModels are the Codex backends advertised before any listing was read (lib/request-worker.js:226-228).
var defaultModels = []string{"gpt-6.1-sol", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-6-astra"}

// dispatchModel is the virtual dispatch row's backend spelling and advertised ID (lib/runtime.js:30).
const (
	dispatchModel   = "auto"
	dispatchModelID = "claude-codex-auto"
)

type catalogRow struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Type        string `json:"type"`
	Window      int64  `json:"max_input_tokens"`
}

// fallbackSnapshot is the catalog a worker serves until its model cache publishes a generation
// (lib/request-worker.js:1211-1214,703-711).
func fallbackSnapshot(lookup config.LookupEnv) route.Snapshot {
	var resolver policy.Resolver
	ids := defaultModels
	if config.ListDispatchModel.Get(lookup) {
		ids = append(append([]string(nil), defaultModels...), dispatchModel)
	}
	rows := make([]catalogRow, 0, len(ids))
	for _, id := range ids {
		if id == dispatchModel {
			rows = append(rows, catalogRow{ID: dispatchModelID, DisplayName: "Sidequest Dispatch (Codex)", Type: "model", Window: resolver.CodexWindow(id)})
			continue
		}
		if m := resolver.Resolve(id); m == nil || m.Backend != "codex" {
			continue
		}
		rows = append(rows, catalogRow{ID: resolver.ClientID(id), DisplayName: codexDisplayName(id), Type: "model", Window: resolver.CodexWindow(id)})
	}
	data, err := json.Marshal(rows)
	if err != nil {
		data = []byte("[]")
	}
	return route.NewSnapshot(resolver, "fallback", jsontext.Value(data), nil)
}

// codexDisplayName applies the source's Codex label rule (lib/request-worker.js:216-220).
func codexDisplayName(id string) string {
	if rest, ok := strings.CutPrefix(id, "gpt-"); ok {
		id = "GPT-" + rest
	}
	return strings.TrimSuffix(id, "[1m]") + " (Codex)"
}
