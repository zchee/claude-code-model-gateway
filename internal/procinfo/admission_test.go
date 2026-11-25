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

package procinfo

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestConstructedProbeAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := New(nil)
		release := make(chan struct{})
		var calls atomic.Int32
		p.sockets = func(context.Context) ([]socketEvidence, error) {
			if calls.Add(1) == 1 {
				<-release
			}
			return nil, nil
		}
		copyOfProbe := p
		ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
		defer cancel()
		if got := p.Listener(ctx, 27123); got.State != ListenerTimedOut {
			t.Errorf("first query: %+v", got)
		}
		if got := copyOfProbe.Listener(t.Context(), 27123); got.State != ListenerUnknown || calls.Load() != 1 {
			t.Errorf("occupied copy: %+v, calls %d", got, calls.Load())
		}
		close(release)
		synctest.Wait()
		if got := copyOfProbe.Listener(t.Context(), 27123); got.State != ListenerAbsent || calls.Load() != 2 {
			t.Errorf("released copy: %+v, calls %d", got, calls.Load())
		}
	})
}

func TestDefaultProbeTimeout(t *testing.T) {
	tests := map[string]struct {
		os   string
		want time.Duration
	}{"success: linux": {"linux", 2 * time.Second}, "success: windows": {"windows", 8 * time.Second}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := DefaultProbeTimeout(tt.os); got != tt.want {
				t.Errorf("got %v want %v", got, tt.want)
			}
		})
	}
}
