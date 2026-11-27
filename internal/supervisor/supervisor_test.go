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
	"io"
	"strings"
	"testing"
	"testing/synctest"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
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

// TestFixtureControlFirstMessageOnly checks that the guarded fixture channel is decided by its first non-blank message
// alone, as the source's one-time message listener is (lib/commands.js:2900-2903). A shutdown as the first message
// stops the supervisor at once, whatever the same write carries after it. After any other first message the channel
// stays open: the source removes its listener but keeps the channel, so a peer that sends later messages, a shutdown
// among them, sees them taken and dropped rather than a closed channel, and the reader reports no shutdown only when
// the peer ends the channel. The peer writes one message at a time into a pipe the test holds, in virtual time, and
// after each write waits until the reader is idle, so the order of the reader's return and the peer's writes is
// decided, not raced.
func TestFixtureControlFirstMessageOnly(t *testing.T) {
	const shutdown = "{\"type\":\"fixture-shutdown\"}\n"
	const noop = "{\"type\":\"noop\"}\n"
	tests := map[string]controlCase{
		"success: shutdown after an unrelated object does not stop":                       {open: []string{noop, shutdown}},
		"success: shutdown after an object without a type does not stop":                  {open: []string{"{\"kind\":\"fixture-shutdown\"}\n", shutdown}},
		"success: shutdown after a non-object message does not stop":                      {open: []string{"[\"fixture-shutdown\"]\n", shutdown}},
		"success: shutdown after two unrelated messages does not stop":                    {open: []string{noop, "{\"type\":\"listening\"}\n", shutdown}},
		"success: shutdown in the same write as an unrelated first message does not stop": {open: []string{noop + shutdown}},
		"success: shutdown after blank lines and an unrelated object does not stop":       {open: []string{"\n", "\n\n", noop, shutdown}},
		"success: shutdown as the first message stops":                                    {deciding: shutdown + noop},
		"success: shutdown as the first message stops before a second one":                {deciding: shutdown + shutdown},
		"success: shutdown as the first message after blank lines stops":                  {open: []string{"\n", "\n\n"}, deciding: shutdown},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			checkControlChannel(t, supervisor.FixtureShutdown, tc)
		})
	}
}

// controlCase is what the peer of the fixture channel writes, one element per write.
type controlCase struct {
	// open are the writes after each of which the reader must still be running, having taken the whole write.
	open []string
	// deciding, when set, is the write after which the reader must report a shutdown. When empty, the peer ends the
	// channel after the open writes, and the reader must then report no shutdown.
	deciding string
}

// controlPipe is the reading end of the fixture channel; it records whether the reader closed it.
type controlPipe struct {
	*io.PipeReader
	closed bool
}

func (c *controlPipe) Close() error { c.closed = true; return c.PipeReader.Close() }

// checkControlChannel runs read on a guarded fixture channel whose peer is the test, and checks it against tc.
func checkControlChannel(t *testing.T, read func(io.ReadCloser, bool) (bool, error), tc controlCase) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		pr, pw := io.Pipe()
		// Closing both ends releases a reader or a writer still blocked when the case has already failed.
		defer func() { _, _ = pr.Close(), pw.Close() }()
		ch := &controlPipe{PipeReader: pr}
		type result struct {
			Stop bool
			Err  error
		}
		results := make(chan result, 1)
		go func() {
			stop, err := read(ch, true)
			results <- result{Stop: stop, Err: err}
		}()
		var got *result
		// returned reports, once every goroutine of the case is idle, whether read has returned.
		returned := func() bool {
			synctest.Wait()
			if got == nil {
				select {
				case r := <-results:
					got = &r
				default:
				}
			}
			return got != nil
		}
		// write hands one write to the reader and reports, once every goroutine is idle, whether the write completed
		// and its error. A write that has not completed is one the reader did not take.
		write := func(text string) (bool, error) {
			written := make(chan error, 1)
			go func() {
				_, err := io.WriteString(pw, text)
				written <- err
			}()
			synctest.Wait()
			select {
			case err := <-written:
				return true, err
			default:
				return false, nil
			}
		}

		for i, text := range tc.open {
			done, err := write(text)
			if !done || err != nil {
				t.Errorf("write %d %q: completed %t, error %v; want the reader to take it from an open channel", i, text, done, err)
				return
			}
			if returned() {
				t.Errorf("reader returned (%t, %v) after write %d %q, before the peer ended the channel", got.Stop, got.Err, i, text)
				return
			}
		}
		if tc.deciding != "" {
			_, _ = write(tc.deciding)
			if !returned() {
				t.Errorf("reader still running after the deciding write %q", tc.deciding)
				return
			}
			if diff := gocmp.Diff(result{Stop: true}, *got, cmpopts.EquateErrors()); diff != "" {
				t.Errorf("result after the deciding write %q (-want +got):\n%s", tc.deciding, diff)
			}
		} else {
			if err := pw.Close(); err != nil {
				t.Fatal(err)
			}
			if !returned() {
				t.Error("reader still running after the peer ended the channel")
				return
			}
			if diff := gocmp.Diff(result{}, *got, cmpopts.EquateErrors()); diff != "" {
				t.Errorf("result after the peer ended the channel (-want +got):\n%s", diff)
			}
		}
		if !ch.closed {
			t.Error("fixture descriptor was not closed")
		}
	})
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
