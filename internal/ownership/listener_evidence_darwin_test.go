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

//go:build process && darwin

package ownership_test

import (
	"context"
	"testing"

	"github.com/zchee/claude-code-model-gateway/internal/ownership"
)

// lookupRecorder records nothing on macOS, which has no proc filesystem to explain a listener answer with.
type lookupRecorder struct{}

// recordLookups returns a unchanged on macOS: nothing is recorded, so the resolver keeps its own evidence source.
func recordLookups(_ *testing.T, a ownership.Authority) (ownership.Authority, *lookupRecorder) {
	return a, nil
}

// listenerEvidence adds nothing on macOS, which has no proc filesystem to read the listener tables from, so a failure
// message there stays as it was.
func listenerEvidence(context.Context, uint16, *lookupRecorder, string) string { return "" }
