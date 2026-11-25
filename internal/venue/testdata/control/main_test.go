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

package control_test

import (
	"os"
	"testing"

	"github.com/zchee/claude-code-model-gateway/internal/venue"
)

func TestMain(m *testing.M) {
	venue.Require()
	os.Exit(m.Run())
}

func writeSentinel(t *testing.T, value string) {
	t.Helper()
	path := os.Getenv("MODEL_GATEWAY_VENUE_SENTINEL")
	if path == "" {
		t.Fatal("MODEL_GATEWAY_VENUE_SENTINEL is unset or empty")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(value + "\n")
	if closeErr := f.Close(); closeErr != nil {
		t.Error(closeErr)
	}
	if err != nil {
		t.Fatal(err)
	}
}
