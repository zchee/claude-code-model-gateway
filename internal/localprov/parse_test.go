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

package localprov_test

import (
	"os"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/localprov"
	"github.com/zchee/claude-code-model-gateway/internal/venue"
)

func TestMain(m *testing.M) {
	venue.Require()
	os.Exit(m.Run())
}

func TestLlamaWindow(t *testing.T) {
	tests := map[string]struct {
		body string
		want int64
	}{
		"success: per-slot window":      {`{"n_ctx":512,"default_generation_settings":{"n_ctx":40960}}`, 40960},
		"success: largest safe integer": {`{"default_generation_settings":{"n_ctx":9007199254740991}}`, 9007199254740991},
		"success: router mode":          {`{"default_generation_settings":{"n_ctx":0}}`, 0},
		"error: missing settings":       {`{"n_ctx":8192}`, 0},
		"error: string":                 {`{"default_generation_settings":{"n_ctx":"40960"}}`, 0},
		"error: fractional":             {`{"default_generation_settings":{"n_ctx":1.5}}`, 0},
		"error: negative":               {`{"default_generation_settings":{"n_ctx":-1}}`, 0},
		"error: beyond safe integer":    {`{"default_generation_settings":{"n_ctx":9007199254740992}}`, 0},
		"error: malformed response":     {`{not json`, 0},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, localprov.LlamaWindow([]byte(tt.body))); diff != "" {
				t.Errorf("LlamaWindow (-want +got):\n%s", diff)
			}
		})
	}
}

func TestStudioModels(t *testing.T) {
	const noList = "GET /api/v1/models returned no model list"
	tests := map[string]struct {
		body      string
		want      map[string]localprov.Model
		wantError string
	}{
		"success: loaded instances": {
			body: `{"models":[
			{"type":"llm","key":"named","loaded_instances":[{"id":"copy","config":{"context_length":8192}},{"id":"named","config":{"context_length":131072}}]},
			{"type":"llm","key":"renamed","loaded_instances":[{"id":"large","config":{"context_length":40960}},{"id":"bad","config":{"context_length":-3}},{"id":"small","config":{"context_length":16384}}]},
			{"type":"llm","key":"unknown","loaded_instances":[{"id":"copy","config":{"context_length":65536}},{"id":"unknown","config":{"context_length":0}}]},
			{"type":"llm","key":"unloaded","max_context_length":131072},
			{"type":"embedding","key":"embedding"},{"type":"llm","key":""},{"type":"llm","key":1},null]}`,
			want: map[string]localprov.Model{"named": {Loaded: true, ContextWindow: 131072}, "renamed": {Loaded: true, ContextWindow: 16384}, "unknown": {Loaded: true}, "unloaded": {}},
		},
		"success: unsafe and nonnumeric windows": {body: `{"models":[{"type":"llm","key":"x","loaded_instances":[{"config":{"context_length":"5"}},{"config":{"context_length":1.5}},{"config":{"context_length":9007199254740992}}]}]}`, want: map[string]localprov.Model{"x": {Loaded: true}}},
		"success: last duplicate key wins":       {body: `{"models":[{"type":"llm","key":"x","loaded_instances":[{"config":{"context_length":40960}}]},{"type":"llm","key":"x","loaded_instances":{}}]}`, want: map[string]localprov.Model{"x": {}}},
		"success: empty catalog":                 {body: `{"models":[]}`, want: map[string]localprov.Model{}},
		"error: missing list":                    {body: `{}`, wantError: noList},
		"error: list is an object":               {body: `{"models":{}}`, wantError: noList},
		"error: non-JSON response":               {body: `<html>`, wantError: noList},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := localprov.StudioModels([]byte(tt.body))
			if tt.wantError != "" {
				if err == nil {
					t.Fatalf("missing error %q", tt.wantError)
				}
				if diff := gocmp.Diff(tt.wantError, err.Error()); diff != "" {
					t.Errorf("error (-want +got):\n%s", diff)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("StudioModels (-want +got):\n%s", diff)
			}
		})
	}
}
