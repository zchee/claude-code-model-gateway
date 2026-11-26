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
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/localprov"
)

func TestCatalogIgnoresIllFormedKeys(t *testing.T) {
	tests := map[string]struct {
		body string
		want []localprov.Model
	}{
		"success: skip lone surrogates, retain literal replacement and order": {
			body: `{"models":[
				{"type":"llm","key":"first"},
				{"type":"llm","key":"\ud800"},
				{"type":"llm","key":"�"},
				{"type":"llm","key":"\udc01","loaded_instances":[{}]},
				{"type":"llm","key":"🐟"},
				{"type":"llm","key":"last"}]}`,
			want: []localprov.Model{{Key: "first"}, {Key: "�"}, {Key: "🐟"}, {Key: "last"}},
		},
		"success: replace raw invalid UTF-8 before parsing": {
			body: `{"models":[{"type":"llm","key":"a` + "\xff" + `b"}]}`,
			want: []localprov.Model{{Key: "a�b"}},
		},
		"success: prefer last duplicate JSON member": {
			body: `{"models":false,"models":[{"type":"embedding","type":"llm","key":"first","key":"last"}]}`,
			want: []localprov.Model{{Key: "last"}},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := localprov.StudioModels([]byte(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("StudioModels (-want +got):\n%s", diff)
			}
		})
	}
}

func TestLlamaWindowNumericSpellings(t *testing.T) {
	tests := map[string]struct {
		body string
		want int64
	}{
		"success: integral decimal":  {`{"default_generation_settings":{"n_ctx":8192.0}}`, 8192},
		"success: integral exponent": {`{"default_generation_settings":{"n_ctx":8.192e3}}`, 8192},
		"success: last duplicate":    {`{"default_generation_settings":{"n_ctx":4096,"n_ctx":8192}}`, 8192},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, localprov.LlamaWindow([]byte(tt.body))); diff != "" {
				t.Errorf("LlamaWindow (-want +got):\n%s", diff)
			}
		})
	}
}
