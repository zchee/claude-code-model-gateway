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

//go:build linux || darwin

package workerlink_test

import (
	"strings"
	"testing"

	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// TestReserveRefusesOversizedGeneration checks a generation far longer than any drain line can carry: Reserve refuses
// it, returns no reservation, and its error does not repeat the oversized value, which can be of any size.
func TestReserveRefusesOversizedGeneration(t *testing.T) {
	tests := map[string]struct {
		size int
	}{
		"error: generation of 64 KiB":                    {size: 64 << 10},
		"error: generation of four times the line limit": {size: 4 * workerlink.DrainLineLimit},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			reg := workerlink.NewRegistry(newFakeListens().listen, (&immediateDials{t: t}).dial)
			generation := strings.Repeat("g", tt.size)
			res, err := reg.Reserve(t.Context(), workerlink.Generation(generation), 41001)
			if res != nil {
				endAtCleanup(t, res)
				t.Error("Reserve returned a reservation for a generation no drain line can carry")
			}
			if err == nil {
				t.Fatal("Reserve accepted the oversized generation")
			}
			if strings.Contains(err.Error(), generation) {
				t.Errorf("the error of the refused Reserve repeats the %d-byte generation (%d bytes of error text); it must keep the oversized value out", tt.size, len(err.Error()))
			}
		})
	}
}
