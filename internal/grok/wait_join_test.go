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

package grok_test

import (
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"go.uber.org/goleak"

	"github.com/zchee/claude-code-model-gateway/internal/grok"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// TestBranchWaitJoinsTheTokenFlight decides what Branch.Wait waits for when the worker calls it at
// shutdown with a credential refresh in flight: Wait does not return while the refresh's issuer
// request is held, it returns once the refresh has ended, its caller has the refresh's result, the
// rotated credential of an exchange is stored, and a later Wait finds nothing left to wait for.
//
// What happens inside the flight between publishing that no refresh is in flight and ending its own
// context is not reachable through the package's exported names, so this test cannot tell a flight
// that finishes that cleanup before it publishes from one that does it afterwards.
func TestBranchWaitJoinsTheTokenFlight(t *testing.T) {
	tests := map[string]struct {
		// hold names the issuer request that is held until the test releases it.
		hold      string
		wantToken string
		wantFile  string
	}{
		"success: Wait while the token exchange is held returns after the rotated credential is stored": {
			hold:      "exchange",
			wantToken: "fixture-rotated-key-1",
			wantFile:  "fixture-rotated-key-1",
		},
		"success: Wait while the issuer discovery is held returns after the refresh has ended": {
			hold:      "discovery",
			wantToken: "fixture-rotated-key-1",
			wantFile:  "fixture-rotated-key-1",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() { goleak.VerifyNone(t) })
			synctest.Test(t, func(t *testing.T) {
				file := filepath.Join(t.TempDir(), "auth.json")
				writeFixtureFile(t, file, expiredAuthJSON())
				issuer := newRotatingIssuer("fixture-refresh-token")
				gate := make(chan struct{})
				release := sync.OnceFunc(func() { close(gate) })
				defer release()

				tr := roundTripFunc(func(req *http.Request) (*http.Response, error) {
					discovery := req.URL.Path == "/.well-known/openid-configuration"
					if (tt.hold == "discovery") == discovery {
						select {
						case <-gate:
						case <-req.Context().Done():
							return nil, req.Context().Err()
						}
					}
					if discovery {
						return discoveryResponse(req), nil
					}
					status, body := issuer.exchange(req)
					return jsonHTTPResponse(req, status, body), nil
				})
				store := mustNewAuthStore(t, grok.AuthOptions{File: file, Transport: tr, Now: func() time.Time { return refreshFixtureNow }})
				client := upstream.NewClient(upstream.Options{})
				defer func() { _ = client.Close() }()
				branch, err := grok.NewBranch(grok.BranchOptions{Auth: store, Client: client})
				if err != nil {
					t.Fatal(err)
				}

				caller := startTokenCall(store, t.Context(), refreshEntryPoints["AccessToken"])
				synctest.Wait()

				waited := make(chan struct{})
				go func() { branch.Wait(); close(waited) }()
				synctest.Wait()
				select {
				case <-waited:
					t.Errorf("Wait returned while the %s of the refresh was still held", tt.hold)
				default:
				}

				release()
				select {
				case <-waited:
				case <-time.After(time.Minute):
					t.Fatal("Wait did not return a minute after the held issuer request was released")
				}
				// Read before anything else in the bubble runs: Wait has returned, so the refresh's
				// credential must already be stored.
				if got := readFileString(t, file); !strings.Contains(got, tt.wantFile) {
					t.Errorf("the refresh's credential was not stored when Wait returned (file has %d bytes)", len(got))
				}
				res := awaitTokenCall(t, "the refresh's caller", caller, time.Minute, release)
				if res.err != nil || res.token != tt.wantToken {
					t.Errorf("caller got (%q, %v), want (%q, nil)", res.token, res.err, tt.wantToken)
				}

				again := make(chan struct{})
				go func() { branch.Wait(); close(again) }()
				synctest.Wait()
				select {
				case <-again:
				default:
					t.Error("a Wait after the refresh ended did not return at once")
					<-again
				}
			})
		})
	}
}
