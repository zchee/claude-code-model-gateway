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

package compose_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
)

const (
	redirectLifetimeLocation = "https://relay.example.test/v1/messages?held=1"
	redirectLifetimeFirst    = "redirect body, first part\n"
	redirectLifetimeSecond   = "redirect body, second part\n"
)

// redirectLifetimeObservation is what one path shows at the fixed points of a redirect whose body
// is held open by the upstream while the client departs or the service shuts down. Compaction
// outcomes are left out: the guarded path keeps one by design and the unguarded path has none.
type redirectLifetimeObservation struct {
	// Before the event, after the first part of the body and several heartbeat intervals.
	BeforeEvent         redirectWireView
	LocationAtHead      []string
	ContactsBeforeEvent int32

	// Once the event has settled.
	HandlerReturnedAfterEvent  bool
	ProviderReturnedAfterEvent bool
	ResultsAfterEvent          []redirectFacts
	WireAfterEvent             redirectWireView

	// Once the upstream was allowed to finish its body.
	HandlerReturnedAfterRelease  bool
	ProviderReturnedAfterRelease bool
	ResultsAfterRelease          []redirectFacts
	WireAfterRelease             redirectWireView
	ContactsAfterRelease         int32
}

func redirectReturned(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func redirectFactsWithoutCompaction(results []outcome.Result) []redirectFacts {
	facts := redirectFactsOf(results)
	for i := range facts {
		facts[i].Compaction = nil
	}
	return facts
}

// observeRedirectLifetime serves body in its own bubble against an upstream that answers a 302,
// sends the first part of its body and holds the rest until released, and records the fixed points
// of event ("depart" or "shutdown"). Every reading is taken when the bubble is idle.
func observeRedirectLifetime(t *testing.T, event, body string) redirectLifetimeObservation {
	t.Helper()
	var obs redirectLifetimeObservation
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		unblock := sync.OnceFunc(func() { close(release) })
		defer unblock()
		providerDone := make(chan struct{})
		var contacts atomic.Int32
		results := newRedirectResults()
		h := newHarness(t, harnessConfig{
			inMemory: true, disableUsage: true, disableTrace: true, heartbeatInterval: 100 * time.Millisecond,
			observeOptions: results.observe,
			proxyHandler: func(w http.ResponseWriter, r *http.Request) {
				defer close(providerDone)
				contacts.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Location", redirectLifetimeLocation)
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(http.StatusFound)
				_, _ = io.WriteString(w, redirectLifetimeFirst)
				w.(http.Flusher).Flush()
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				_, _ = io.WriteString(w, redirectLifetimeSecond)
			},
		})

		rec := newRedirectWireRecorder()
		ctx, cancel := context.WithCancel(t.Context())
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/messages", strings.NewReader(body))
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer func() {
				if p := recover(); p != nil {
					if err, ok := p.(error); !ok || !errors.Is(err, http.ErrAbortHandler) {
						panic(p)
					}
				}
			}()
			h.workerHandler.ServeHTTP(rec, req)
		}()
		t.Cleanup(func() {
			unblock()
			cancel()
			h.cancelService()
			waitSignal(t, "redirect handler cleanup", done, time.Second)
		})

		synctest.Wait()
		// Several heartbeat intervals pass on the bubble's clock while the body is held open.
		time.Sleep(350 * time.Millisecond)
		synctest.Wait()
		obs.BeforeEvent = rec.view()
		obs.LocationAtHead = rec.firstHeadHeader("Location")
		obs.ContactsBeforeEvent = contacts.Load()

		switch event {
		case "depart":
			rec.markDeparted()
			cancel()
		case "shutdown":
			h.cancelService()
		default:
			t.Fatalf("unknown event %q", event)
		}
		synctest.Wait()
		time.Sleep(350 * time.Millisecond)
		synctest.Wait()
		obs.HandlerReturnedAfterEvent = redirectReturned(done)
		obs.ProviderReturnedAfterEvent = redirectReturned(providerDone)
		obs.ResultsAfterEvent = redirectFactsWithoutCompaction(results.snapshot())
		obs.WireAfterEvent = rec.view()

		unblock()
		synctest.Wait()
		time.Sleep(350 * time.Millisecond)
		synctest.Wait()
		obs.HandlerReturnedAfterRelease = redirectReturned(done)
		obs.ProviderReturnedAfterRelease = redirectReturned(providerDone)
		obs.ResultsAfterRelease = redirectFactsWithoutCompaction(results.snapshot())
		obs.WireAfterRelease = rec.view()
		obs.ContactsAfterRelease = contacts.Load()
	})
	return obs
}

// TestGuardedRedirectLifetimeMatchesUnguarded decides that a client departure or a service shutdown
// during the relay of a guarded compaction request's redirect body behaves as on the unguarded
// path: the same bytes reached the client, the same writes happened after the event, the handler
// and the upstream end at the same points, and the same terminal facts are delivered. Before the
// event, both paths have relayed the head and the first part of the body as they arrived, with no
// heartbeat, after one upstream contact.
func TestGuardedRedirectLifetimeMatchesUnguarded(t *testing.T) {
	tests := map[string]struct{ event string }{
		"success: the client departs while the redirect body is held open":     {event: "depart"},
		"success: the service shuts down while the redirect body is held open": {event: "shutdown"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			unguarded := observeRedirectLifetime(t, tt.event, messageRequestJSON(redirectModel, true))
			guarded := observeRedirectLifetime(t, tt.event, compactRequestJSON(redirectModel))

			wantBefore := redirectWireView{Heads: []int{http.StatusFound}, Body: redirectLifetimeFirst}
			paths := map[string]redirectLifetimeObservation{"guarded": guarded, "unguarded": unguarded}
			for label, obs := range paths {
				if diff := gocmp.Diff(wantBefore, obs.BeforeEvent); diff != "" {
					t.Errorf("%s: heads and body before the event (-want +got):\n%s", label, diff)
				}
				if diff := gocmp.Diff([]string{redirectLifetimeLocation}, obs.LocationAtHead); diff != "" {
					t.Errorf("%s: Location committed with the head (-want +got):\n%s", label, diff)
				}
				if diff := gocmp.Diff(int32(1), obs.ContactsBeforeEvent); diff != "" {
					t.Errorf("%s: upstream contacts before the event (-want +got):\n%s", label, diff)
				}
				if diff := gocmp.Diff(int32(1), obs.ContactsAfterRelease); diff != "" {
					t.Errorf("%s: upstream contacts after the upstream finished (-want +got):\n%s", label, diff)
				}
			}
			if diff := gocmp.Diff(unguarded, guarded); diff != "" {
				t.Errorf("observations, guarded against unguarded (-unguarded +guarded):\n%s", diff)
			}
		})
	}
}
