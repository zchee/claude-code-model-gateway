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

// Package localprov interprets local provider responses without issuing requests.
package localprov

import "errors"

// LlamaWindow returns the per-slot context from a llama-server props response, or zero if unknown.
func LlamaWindow(body []byte) int64 { return 0 }

// Model describes a downloaded LM Studio model. A zero window means no context was measured.
type Model struct {
	Key           string
	Loaded        bool
	ContextWindow int64
}

// StudioModels returns models in catalog order, or an error when the response has no list.
// A repeated key updates the model at its first position.
func StudioModels(body []byte) ([]Model, error) { return nil, errors.New("not implemented") }
