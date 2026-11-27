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

package relay_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/supervisor/relay"
	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// built is the result of one relay.New call.
type built struct {
	h   http.Handler
	err error
	// panicked is the value relay.New panicked with, or nil.
	panicked any
}

// construct calls relay.New and reports a panic as a value, so that one failing case does not end the test binary.
func construct(o relay.Options) (b built) {
	defer func() { b.panicked = recover() }()
	b.h, b.err = relay.New(o)
	return b
}

// newRelay calls relay.New and turns a panic or an error into a failure of this test only.
func newRelay(t *testing.T, o relay.Options) http.Handler {
	t.Helper()
	b := construct(o)
	switch {
	case b.panicked != nil:
		t.Fatalf("relay.New panicked: %v", b.panicked)
	case b.err != nil:
		t.Fatalf("relay.New: %v", b.err)
	case b.h == nil:
		t.Fatal("relay.New returned no handler and no error")
	}
	return b.h
}

// idleSource never has an eligible worker; New must not call it.
type idleSource struct{ t *testing.T }

func (s idleSource) Acquire(context.Context, workerlink.Generation) (workerlink.Lease, error) {
	s.t.Error("New acquired a worker lease")
	return nil, workerlink.ErrUnavailable
}

// TestNew checks that the relay cannot be built without its worker source or its wait, and is built from complete
// options. Neither field has a default: the worker source is the only way to reach a worker, and Wait carries the
// supervisor's stop signal as well as the pause, which a default timer would lose. The godoc of Options.Wait states the
// refusal.
func TestNew(t *testing.T) {
	wait := func(context.Context, time.Duration) error { return nil }
	lost := func(relay.Lost) {}
	tests := map[string]struct {
		options func(t *testing.T) relay.Options
		wantErr bool
	}{
		"success: complete options": {
			options: func(t *testing.T) relay.Options {
				return relay.Options{Workers: idleSource{t}, Lost: lost, Wait: wait, Version: "1.2.3", SupervisorPID: 4242}
			},
		},
		"error: no worker source": {
			options: func(*testing.T) relay.Options {
				return relay.Options{Lost: lost, Wait: wait, Version: "1.2.3", SupervisorPID: 4242}
			},
			wantErr: true,
		},
		"error: no wait between attempts": {
			options: func(t *testing.T) relay.Options {
				return relay.Options{Workers: idleSource{t}, Lost: lost, Version: "1.2.3", SupervisorPID: 4242}
			},
			wantErr: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			b := construct(tt.options(t))
			if b.panicked != nil {
				t.Fatalf("relay.New panicked: %v", b.panicked)
			}
			if tt.wantErr {
				if b.err == nil {
					t.Error("relay.New succeeded; want an error")
				}
				if b.h != nil {
					t.Error("relay.New returned a handler together with its refusal")
				}
				return
			}
			if b.err != nil || b.h == nil {
				t.Errorf("relay.New = %v, %v; want a handler and no error", b.h, b.err)
			}
		})
	}
}

// TestLimits pins the source's upload bound and oversize grace (lib/commands.js:2289,2302).
func TestLimits(t *testing.T) {
	tests := map[string]struct {
		got, want any
	}{
		"success: the upload bound is 64 MiB":   {got: relay.BodyLimit, want: 67108864},
		"success: the oversize grace is 250 ms": {got: relay.OversizeGrace, want: 250 * time.Millisecond},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, tt.got); diff != "" {
				t.Errorf("(-want +got):\n%s", diff)
			}
		})
	}
}
