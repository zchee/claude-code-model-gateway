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

// Package toolschema adapts tool input schemas for Codex.
package toolschema

import (
	"errors"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

// Edit records a replacement pattern and its RFC 6901 pointer rooted at /tools.
type Edit struct {
	Pointer     string `json:"pointer"`
	Replacement string `json:"replacement"`
}

// CompatibilityError identifies the tool and schema location that prevented adaptation.
type CompatibilityError struct {
	ReasonCode string `json:"reasonCode"`
	Pointer    string `json:"pointer"`
	ToolName   string `json:"toolName"`
}

// Error describes the refusal for the caller.
func (e *CompatibilityError) Error() string {
	return "Codex schema compatibility refused " + e.ToolName + " at " + e.Pointer + " (" + e.ReasonCode + ")"
}

// Adapt rewrites eligible tool schemas without mutating tools. Unchanged containers
// retain their original backing slices. Edits follow traversal order. A refusal
// returns no output or edits and identifies the first incompatible schema.
func Adapt(tools jsonx.WireValue) (jsonx.WireValue, []Edit, error) {
	return jsonx.WireValue{}, nil, errors.New("tool schema adaptation not implemented")
}

func containsPotentialProperty(pattern []uint16) bool { return false }

func acceptedEscapeKinds() []string { return nil }

func validUnicodePattern(pattern []uint16) bool { return false }
