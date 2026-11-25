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

// Package anthropiccatalog persists upstream catalog snapshots and merges picker rows.
package anthropiccatalog

import (
	"encoding/json/jsontext"
	"errors"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/policy"
)

// Read returns a normalized cache object, retaining unknown fields and their ordering.
// A missing or malformed cache supplies empty data and a zero update time.
func Read(path string) jsontext.Value { return nil }

// WriteSuccess stores valid upstream rows with the supplied refresh time and clears old errors.
func WriteSuccess(path string, rows jsontext.Value, at time.Time) error {
	return errors.New("catalog write not implemented")
}

// WriteFailure records the failed attempt against the snapshot that triggered the refresh.
func WriteFailure(path string, snapshot jsontext.Value, message string, at time.Time) error {
	return errors.New("catalog write not implemented")
}

// WithCached puts the advertised cached rows before models, which lose on an ID collision.
func WithCached(models, cachedRows jsontext.Value, resolver policy.Resolver) jsontext.Value {
	return nil
}

// WithPrevious prepends relabelled older server rows, replacing any models with those IDs.
// Server rows are the entries of Claude Code's additionalModelOptionsCache array.
func WithPrevious(models, serverRows jsontext.Value, currentPins map[string]string) jsontext.Value {
	return nil
}
