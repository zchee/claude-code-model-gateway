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

package supervisor_test

import (
	"bufio"
	"bytes"
	json "encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/controlauth"
	"github.com/zchee/claude-code-model-gateway/internal/supervisor"
	"github.com/zchee/claude-code-model-gateway/internal/worker"
	workeroutcome "github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
)

// TestControlPathsMatchTheRawTarget decides that the control paths are matched on the raw request target before
// '?': an encoded or otherwise different spelling is ordinary traffic for the relay, never a proof or a stop.
func TestControlPathsMatchTheRawTarget(t *testing.T) {
	f := ctlServe(t, nil)
	nonce := ctlNonce(t)
	tests := map[string]string{
		"success: an encoded letter in the identity path":   ctlHead("GET /internal/%69dentity HTTP/1.1", ctlHost(f.port), ctlNonceName+": "+nonce),
		"success: a trailing slash on the identity path":    ctlHead("GET /internal/identity/ HTTP/1.1", ctlHost(f.port), ctlNonceName+": "+nonce),
		"success: a doubled slash before the identity path": ctlHead("GET //internal/identity HTTP/1.1", ctlHost(f.port), ctlNonceName+": "+nonce),
		"success: an encoded letter in the stop path": ctlHead("POST /internal/%73top HTTP/1.1", ctlHost(f.port), ctlTokenName+": "+ctlToken,
			"Content-Type: application/json", "Content-Length: 2") + "{}",
	}
	relays := int64(0)
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			c := ctlDial(t, f.port)
			resp, body := c.exchange(t, raw)
			relays++
			if resp.StatusCode != http.StatusOK || string(body) != "relayed" {
				t.Errorf("got %d %q; want the relay's answer", resp.StatusCode, body)
			}
			if n := f.relays.Load(); n != relays {
				t.Errorf("relay reached %d times after %d requests", n, relays)
			}
		})
	}
	if diff := gocmp.Diff([]ctlSigned(nil), f.signatures()); diff != "" {
		t.Errorf("other spellings reached the signer (-want +got):\n%s", diff)
	}
	if n := f.stops.Load(); n != 0 {
		t.Errorf("other spellings invoked Stop %d times", n)
	}
}

// TestControlProofNamesItsOwnStartupGeneration decides, with two independently configured supervisors, that each
// signs the startup generation it was given and no other.
func TestControlProofNamesItsOwnStartupGeneration(t *testing.T) {
	generations := map[string]string{"success: the first supervisor": "startup-first-91c2", "success: the second supervisor": "startup-second-07ad"}
	for name, generation := range generations {
		t.Run(name, func(t *testing.T) {
			f := ctlServe(t, func(_ *ctlFixture, o *supervisor.Options) { o.StartupGeneration = generation })
			c := ctlDial(t, f.port)
			nonce := ctlNonce(t)
			resp, body := c.exchange(t, ctlIdentityRequest(f.port, nonce))
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d", resp.StatusCode)
			}
			var answer ctlIdentityAnswer
			if err := json.Unmarshal(body, &answer, json.RejectUnknownMembers(true)); err != nil {
				t.Fatalf("identity body %q: %v", body, err)
			}
			if answer.Generation != generation {
				t.Errorf("answered generation %q; want %q", answer.Generation, generation)
			}
			for other := range generations {
				if g := generations[other]; g != generation && controlauth.VerifySupervisorIdentity(ctlToken, nonce, g, answer.Proof) {
					t.Errorf("the proof verifies for the other supervisor's generation %q", g)
				}
			}
			if !controlauth.VerifySupervisorIdentity(ctlToken, nonce, generation, answer.Proof) {
				t.Errorf("the proof does not verify for its own generation %q", generation)
			}
		})
	}
}

// ctlHexRun finds every run of 64 lowercase hexadecimal characters, the spelling of a proof.
var ctlHexRun = regexp.MustCompile(`[0-9a-f]{64}`)

// ctlCollect walks a decoded JSON value and gathers every string it holds, keyed or not, and every generation
// member's value, however the document spelled its names and strings.
func ctlCollect(v any, strs, generations *[]string) {
	switch v := v.(type) {
	case map[string]any:
		for k, child := range v {
			if g, ok := child.(string); ok && k == "generation" {
				*generations = append(*generations, g)
			}
			ctlCollect(child, strs, generations)
		}
	case []any:
		for _, child := range v {
			ctlCollect(child, strs, generations)
		}
	case string:
		*strs = append(*strs, v)
	}
}

// ctlNoProof fails if a recorded response, of any status, discloses a proof that verifies for this root's token and
// the challenge's nonce: in its decoded JSON body, in its raw body or in any header value, with any generation the
// body names or the supervisor's own. A response that cannot be serialized fails the test.
func ctlNoProof(t *testing.T, rec *httptest.ResponseRecorder, nonce string) {
	t.Helper()
	resp := rec.Result()
	defer func() { _ = resp.Body.Close() }()
	var raw bytes.Buffer
	if err := resp.Write(&raw); err != nil {
		t.Fatalf("serializing the response: %v", err)
	}
	body := rec.Body.Bytes()
	generations := []string{ctlGeneration}
	var candidates []string
	var decoded any
	if json.Unmarshal(body, &decoded) == nil {
		ctlCollect(decoded, &candidates, &generations)
	}
	candidates = append(candidates, ctlHexRun.FindAllString(string(body), -1)...)
	for _, values := range resp.Header {
		for _, v := range values {
			candidates = append(candidates, ctlHexRun.FindAllString(v, -1)...)
		}
	}
	for _, c := range candidates {
		for _, g := range generations {
			if g != "" && controlauth.VerifySupervisorIdentity(ctlToken, nonce, g, c) {
				t.Errorf("a handler other than the supervisor's dispatcher disclosed a proof for generation %q: %q", g, raw.String())
			}
		}
	}
}

func ctlParse(t *testing.T, raw string) *http.Request {
	t.Helper()
	req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatal(err)
	}
	return req.WithContext(t.Context())
}

// TestControlOnlyTheSupervisorServesTheProof decides that the worker's handler and the supervisor's legacy handler
// have no route to a proof and no stop: an identity challenge gets no proof and an authenticated stop has no
// effect. Both hold the same control token, so this is a property of their routes, not of the key.
func TestControlOnlyTheSupervisorServesTheProof(t *testing.T) {
	const port = 18764
	nonce := ctlNonce(t)
	identity := ctlIdentityRequest(port, nonce)
	stop := ctlStopRequest(port, "{}")
	t.Run("success: the request worker", func(t *testing.T) {
		var forwarded, drains atomic.Int64
		forward := func(w http.ResponseWriter, _ *http.Request, _ route.Snapshot, _ route.Decision, _ []byte, _ func(workeroutcome.Route, *workeroutcome.Audit)) (workeroutcome.Result, error) {
			forwarded.Add(1)
			w.WriteHeader(http.StatusBadGateway)
			return workeroutcome.Result{}, nil
		}
		h, err := worker.NewHandler(worker.Options{
			Version: "1.2.3", ControlToken: ctlToken, Anthropic: forward, Proxy: forward, Grok: forward,
			BeginDrain: func() { drains.Add(1) },
		})
		if err != nil {
			t.Fatalf("worker handler: %v", err)
		}
		for name, raw := range map[string]string{"identity": identity, "stop": stop} {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, ctlParse(t, raw))
			ctlNoProof(t, rec, nonce)
			if rec.Code == http.StatusAccepted && strings.Contains(rec.Body.String(), "stopping") {
				t.Errorf("the worker acknowledged a %s request as a stop: %q", name, rec.Body.String())
			}
		}
		if n := drains.Load(); n != 0 {
			t.Errorf("the worker began %d drains", n)
		}
	})
	t.Run("success: the supervisor's legacy handler", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h, err := supervisor.NewHandler(port, func() int { return 0 }, ctlToken, "1.2.3", 4242)
			if err != nil {
				t.Fatalf("legacy handler: %v", err)
			}
			for name, raw := range map[string]string{"identity": identity, "stop": stop} {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, ctlParse(t, raw))
				ctlNoProof(t, rec, nonce)
				if rec.Code == http.StatusAccepted {
					t.Errorf("the legacy handler acknowledged a %s request: %q", name, rec.Body.String())
				}
			}
		})
	})
}
