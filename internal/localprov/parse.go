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

import (
	"errors"
	"math"
	"slices"
	"unicode/utf16"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

// LlamaWindow returns the per-slot context from a llama-server props response, or zero if unknown.
func LlamaWindow(body []byte) int64 {
	props, err := jsonx.DecodeWire(body)
	if err != nil {
		return 0
	}
	return contextWindow(property(property(props, "default_generation_settings"), "n_ctx"))
}

// Model describes a downloaded LM Studio model. A zero window means no context was measured.
type Model struct {
	Key           string
	Loaded        bool
	ContextWindow int64
}

// StudioModels returns models in catalog order, or an error when the response has no list.
// A repeated key updates the model at its first position. Like Node, the decoder
// replaces invalid UTF-8 bytes with U+FFFD, so those keys match literal U+FFFD.
// Keys containing escaped unpaired surrogates are dropped.
func StudioModels(body []byte) ([]Model, error) {
	catalog, err := jsonx.DecodeWire(body)
	list := property(catalog, "models")
	if err != nil || list.Kind != '[' {
		return nil, errors.New("GET /api/v1/models returned no model list")
	}

	models := []Model{}
	positions := make(map[string]int)
	for _, entry := range list.Elements {
		kind := property(entry, "type")
		key := property(entry, "key")
		if !asciiEqual(kind.Text, "llm") || key.Kind != '"' || len(key.Text) == 0 || !wellFormedUTF16(key.Text) {
			continue
		}
		instances := property(entry, "loaded_instances").Elements
		model := Model{
			Key:           string(utf16.Decode(key.Text)),
			Loaded:        len(instances) > 0,
			ContextWindow: modelWindow(key.Text, instances),
		}
		if pos, ok := positions[model.Key]; ok {
			models[pos] = model
			continue
		}
		positions[model.Key] = len(models)
		models = append(models, model)
	}
	return models, nil
}

func modelWindow(key []uint16, instances []jsonx.WireValue) int64 {
	var smallest int64
	for _, instance := range instances {
		window := contextWindow(property(property(instance, "config"), "context_length"))
		if slices.Equal(property(instance, "id").Text, key) {
			// The named instance wins even if its context is unknown.
			return window
		}
		if window > 0 && (smallest == 0 || window < smallest) {
			smallest = window
		}
	}
	return smallest
}

func contextWindow(value jsonx.WireValue) int64 {
	n := value.Number
	if n > 0 && n <= 1<<53-1 && math.Trunc(n) == n {
		return int64(n)
	}
	return 0
}

func property(value jsonx.WireValue, name string) jsonx.WireValue {
	for _, member := range value.Members {
		if asciiEqual(member.Name, name) {
			return member.Value
		}
	}
	return jsonx.WireValue{}
}

func asciiEqual(units []uint16, text string) bool {
	return slices.EqualFunc(units, []byte(text), func(unit uint16, b byte) bool { return unit == uint16(b) })
}

func wellFormedUTF16(units []uint16) bool {
	for i := 0; i < len(units); i++ {
		if r := rune(units[i]); utf16.IsSurrogate(r) {
			if i+1 == len(units) || utf16.DecodeRune(r, rune(units[i+1])) == '�' {
				return false
			}
			i++
		}
	}
	return true
}
