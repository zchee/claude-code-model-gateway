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

//go:build darwin || (linux && process)

package procinfo

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"golang.org/x/sys/unix"
)

func TestGroupSignalRequiresFreshChildObservation(t *testing.T) {
	tests := map[string]struct {
		exited bool
		cause  error
	}{
		"error: exited leader reaped before signal":               {true, unix.ECHILD},
		"error: exited leader unreadable before signal":           {true, unix.EIO},
		"error: running leader reaped before timeout cleanup":     {false, unix.ECHILD},
		"error: running leader unreadable before timeout cleanup": {false, unix.EIO},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
				defer cancel()
				const pid = 701
				var calls, signals int
				g := commandGroup{observe: func(got int) (bool, error) {
					calls++
					if got != pid {
						t.Fatalf("observed pid %d, want %d", got, pid)
					}
					if calls == 1 {
						return tt.exited, nil
					}
					return false, tt.cause
				}, signal: func(int, unix.Signal) error { signals++; return nil }}
				err := g.stop(ctx, pid)
				if !errors.Is(err, ErrGroupCleanup) || !errors.Is(err, tt.cause) || signals != 0 || calls != 2 {
					t.Fatalf("cleanup: err=%v, observations=%d, group signals=%d", err, calls, signals)
				}
				if errors.Is(ctx.Err(), context.DeadlineExceeded) == tt.exited {
					t.Fatal("test did not reach the selected cleanup branch")
				}
			})
		})
	}
}
