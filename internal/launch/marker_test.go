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

package launch_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/launch"
)

// markTFence is a counting acquire function with its publication: it logs every call of acquire, publish and release
// in the order they happen, and fails the steps it is given errors for. A failed acquisition still hands out a
// release, so that a call of it after the failure is logged too. caller is the context the caller of the fenced
// publication passes; acquire must be given that very context, compared by identity, so that a derived context
// (one detached from the caller's cancellation, for example) is logged as another context.
type markTFence struct {
	caller                             context.Context
	acquireErr, publishErr, releaseErr error
	held                               bool
	log                                []string
}

func (f *markTFence) acquire(ctx context.Context) (func() error, error) {
	entry := "acquire"
	if ctx != f.caller {
		entry = "acquire with another context than the caller's"
	}
	f.log = append(f.log, entry)
	f.held = f.acquireErr == nil
	return f.release, f.acquireErr
}

func (f *markTFence) publish() error {
	if f.held {
		f.log = append(f.log, "publish")
	} else {
		f.log = append(f.log, "publish without the fence")
	}
	return f.publishErr
}

func (f *markTFence) release() error {
	if f.held {
		f.log = append(f.log, "release")
	} else {
		f.log = append(f.log, "release of a fence that is not held")
	}
	f.held = false
	return f.releaseErr
}

// markTPublish runs the fenced publication once and recovers a panic as a failure of the calling row alone.
func markTPublish(t *testing.T, ctx context.Context, f *markTFence) (ok bool, err error) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Errorf("PublishUnderFence panicked: %v", p)
			ok = false
		}
	}()
	return true, launch.PublishUnderFence(ctx, f.acquire, f.publish)
}

func TestPublishUnderFence(t *testing.T) {
	errAcquire := errors.New("acquire failed")
	errPublish := errors.New("publish failed")
	errRelease := errors.New("release failed")
	tests := map[string]struct {
		acquireErr, publishErr, releaseErr error
		// wantLog is the order of the calls; wantErr is the returned error's text, "" for nil; wantMatches lists the
		// injected errors that errors.Is must find in it, and every other injected error must not be found.
		wantLog     []string
		wantErr     string
		wantMatches []error
	}{
		"success: publishes while the fence is held and releases it exactly once": {
			wantLog: []string{"acquire", "publish", "release"},
		},
		"error: a failed acquisition is returned, with nothing published and nothing released": {
			acquireErr:  errAcquire,
			wantLog:     []string{"acquire"},
			wantErr:     "acquire failed",
			wantMatches: []error{errAcquire},
		},
		"error: a failed publication is returned, and the fence is still released exactly once": {
			publishErr:  errPublish,
			wantLog:     []string{"acquire", "publish", "release"},
			wantErr:     "publish failed",
			wantMatches: []error{errPublish},
		},
		"error: a release that fails after a successful publication is returned alone": {
			releaseErr:  errRelease,
			wantLog:     []string{"acquire", "publish", "release"},
			wantErr:     "release failed",
			wantMatches: []error{errRelease},
		},
		"error: when publication and release both fail, the publication's error comes first and the release error is joined after it": {
			publishErr:  errPublish,
			releaseErr:  errRelease,
			wantLog:     []string{"acquire", "publish", "release"},
			wantErr:     "publish failed\nrelease failed",
			wantMatches: []error{errPublish, errRelease},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			f := &markTFence{caller: ctx, acquireErr: tt.acquireErr, publishErr: tt.publishErr, releaseErr: tt.releaseErr}
			ok, err := markTPublish(t, ctx, f)
			if !ok {
				return
			}
			if diff := gocmp.Diff(tt.wantLog, f.log); diff != "" {
				t.Errorf("calls of acquire, publish and release (-want +got):\n%s", diff)
			}
			got := ""
			if err != nil {
				got = err.Error()
			}
			if (err == nil) != (tt.wantErr == "") || got != tt.wantErr {
				t.Errorf("PublishUnderFence() = %q (nil: %t), want %q", got, err == nil, tt.wantErr)
			}
			for _, injected := range []error{errAcquire, errPublish, errRelease} {
				want := slices.Contains(tt.wantMatches, injected)
				if errors.Is(err, injected) != want {
					t.Errorf("errors.Is(%v, %q) = %t, want %t", err, injected, !want, want)
				}
			}
		})
	}
}
