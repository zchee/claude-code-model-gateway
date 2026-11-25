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

// Package grok translates between Anthropic messages and Grok Responses wire values.
package grok

import (
	"errors"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/sse"
)

// TranslateRequest builds a Responses input for the backend model.
// A null entry in messages returns a zero WireValue and an error instead of a partial request.
func TranslateRequest(payload jsonx.WireValue, model string) (jsonx.WireValue, error) {
	return jsonx.WireValue{}, errors.New("request translation not implemented")
}

// TranslateResponse builds a message for the advertised model, using now for missing IDs.
func TranslateResponse(response jsonx.WireValue, model string, now func() time.Time) jsonx.WireValue {
	return jsonx.WireValue{Kind: 'n'}
}

// Stream holds translation state for one Responses event stream.
type Stream struct{}

// NewStream starts a translator for the advertised model, using now for missing IDs.
func NewStream(model string, now func() time.Time) *Stream { return &Stream{} }

// Frame translates one scanned upstream frame into zero or more complete downstream frames.
// Unterminated tails do not dispatch events.
func (s *Stream) Frame(frame sse.Frame) string { return "" }

// End emits any final frames on orderly upstream EOF. Do not call End on transport failure.
func (s *Stream) End() string { return "" }
