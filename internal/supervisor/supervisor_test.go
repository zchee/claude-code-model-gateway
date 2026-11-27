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
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
	"go.uber.org/goleak"

	"github.com/zchee/claude-code-model-gateway/internal/supervisor"
	"github.com/zchee/claude-code-model-gateway/internal/venue"
)

func TestMain(m *testing.M) {
	venue.Require()
	goleak.VerifyTestMain(m)
}

type fixtureChannel struct {
	*strings.Reader
	closed bool
}

func (c *fixtureChannel) Close() error { c.closed = true; return nil }

func TestFixtureControlRequiresGuard(t *testing.T) {
	const shutdown = "{\"type\":\"fixture-shutdown\"}\n"
	tests := map[string]struct {
		guarded  bool
		input    string
		wantStop bool
	}{
		"success: fixture message stops guarded listener":        {true, shutdown, true},
		"success: production closes the channel without reading": {false, shutdown, false},
		"success: unrelated message is not a stop request":       {true, "{\"type\":\"noop\"}\n", false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			ch := &fixtureChannel{Reader: strings.NewReader(tc.input)}
			stop, err := supervisor.FixtureShutdown(ch, tc.guarded)
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(tc.wantStop, stop); diff != "" {
				t.Errorf("shutdown (-want +got): %s", diff)
			}
			if !ch.closed {
				t.Error("fixture descriptor was not closed")
			}
			if !tc.guarded && ch.Len() != len(tc.input) {
				t.Error("production read from the fixture descriptor")
			}
		})
	}
}

// TestFixtureControlFirstMessageOnly checks that the guarded fixture channel acts on its first message alone, as the
// source's one-time message listener does (lib/commands.js:2900-2903): a shutdown message that follows an unrelated
// first message does not stop the supervisor, and one that comes first does, whatever follows it. Each input ends at
// end of file, so a reader that stops after the first message and one that reads on to the end both return.
func TestFixtureControlFirstMessageOnly(t *testing.T) {
	const shutdown = "{\"type\":\"fixture-shutdown\"}\n"
	tests := map[string]struct {
		input    string
		wantStop bool
	}{
		"success: shutdown after an unrelated object does not stop":        {"{\"type\":\"noop\"}\n" + shutdown, false},
		"success: shutdown after an object without a type does not stop":   {"{\"kind\":\"fixture-shutdown\"}\n" + shutdown, false},
		"success: shutdown after a non-object message does not stop":       {"[\"fixture-shutdown\"]\n" + shutdown, false},
		"success: shutdown after two unrelated messages does not stop":     {"{\"type\":\"noop\"}\n{\"type\":\"listening\"}\n" + shutdown, false},
		"success: shutdown as the first message stops":                     {shutdown + "{\"type\":\"noop\"}\n", true},
		"success: shutdown as the first message stops before a second one": {shutdown + shutdown, true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			ch := &fixtureChannel{Reader: strings.NewReader(tc.input)}
			stop, err := supervisor.FixtureShutdown(ch, true)
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(tc.wantStop, stop); diff != "" {
				t.Errorf("shutdown for channel input %q (-want +got): %s", tc.input, diff)
			}
			if !ch.closed {
				t.Error("fixture descriptor was not closed")
			}
		})
	}
}

func TestListenerNeedsControlToken(t *testing.T) {
	tests := map[string]struct {
		token string
		valid bool
	}{
		"error: absent token":  {},
		"success: valid token": {strings.Repeat("cd", 32), true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			h, err := supervisor.NewHandler(43501, func() int { return 0 }, tc.token, "1.2.3", 19)
			if tc.valid && (err != nil || h == nil) {
				t.Fatalf("valid token: handler present=%t, err=%v", h != nil, err)
			}
			if !tc.valid && (err == nil || h != nil) {
				t.Fatalf("empty token: handler present=%t, err=%v", h != nil, err)
			}
		})
	}
}
