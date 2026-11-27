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
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/supervisor"
)

var (
	errScriptedChannel = errors.New("fixture channel connection reset")
	errReadAfterError  = errors.New("fixture channel read again after it returned an error")
	// errWrappedBufferFull is a channel error that wraps the sentinel a bufio.Reader uses for
	// its own full buffer; it is still an error of the channel, not its end.
	errWrappedBufferFull = fmt.Errorf("fixture channel: %w", bufio.ErrBufferFull)
)

// longUnrelatedMessage is a message that is not the shutdown message and is longer than a
// bufio.Reader's default buffer of 4096 bytes, so it cannot arrive in one buffered read.
var longUnrelatedMessage = "{\"type\":\"noop\",\"padding\":\"" + strings.Repeat("x", 2*4096) + "\"}\n"

// scriptedChunk is one piece of what a scripted channel delivers: its bytes, and the error the
// Read that hands out the last of those bytes returns with them.
type scriptedChunk struct {
	data string
	err  error
}

// scriptedChannel is a fixture channel that delivers its chunks in order, as many bytes per Read
// as the caller's buffer takes, and then reports a clean end. After it has returned a non-EOF
// error, any further Read fails the test and returns errReadAfterError instead of blocking.
type scriptedChannel struct {
	t      *testing.T
	chunks []scriptedChunk
	// offset is the number of bytes of chunks[0] already delivered.
	offset int
	// failed records that a Read returned a non-EOF error.
	failed bool
	// reads counts every Read; readsAfterError counts those after a non-EOF error.
	reads, readsAfterError int
	closes                 int
}

func (c *scriptedChannel) Read(p []byte) (int, error) {
	c.reads++
	if c.failed {
		c.readsAfterError++
		c.t.Errorf("fixture channel read again (read %d) after it returned an error", c.reads)
		return 0, errReadAfterError
	}
	for len(c.chunks) > 0 {
		chunk := c.chunks[0]
		rest := chunk.data[c.offset:]
		if len(rest) > 0 && len(p) == 0 {
			return 0, nil
		}
		n := copy(p, rest)
		c.offset += n
		if c.offset < len(chunk.data) {
			return n, nil
		}
		c.chunks = c.chunks[1:]
		c.offset = 0
		if chunk.err != nil {
			if !errors.Is(chunk.err, io.EOF) {
				c.failed = true
			}
			return n, chunk.err
		}
		if n > 0 {
			return n, nil
		}
	}
	return 0, io.EOF
}

func (c *scriptedChannel) Close() error { c.closes++; return nil }

// TestFixtureChannelErrorAfterUnrelatedMessage checks a guarded test control channel whose
// first message is not the shutdown message: a channel error that is not its end, arriving
// with that message or after it, is returned with a false stop, and the channel is not read
// again after it. A clean end after the same message stops nothing and reports no error.
func TestFixtureChannelErrorAfterUnrelatedMessage(t *testing.T) {
	const unrelated = "{\"type\":\"noop\"}\n"
	tests := map[string]struct {
		chunks  []scriptedChunk
		wantErr error
	}{
		"error: channel error in the same read as the first message": {
			chunks:  []scriptedChunk{{data: unrelated, err: errScriptedChannel}},
			wantErr: errScriptedChannel,
		},
		"error: channel error in the read after the first message": {
			chunks:  []scriptedChunk{{data: unrelated}, {err: errScriptedChannel}},
			wantErr: errScriptedChannel,
		},
		"error: channel error wrapping the buffer-full sentinel in the same read as the first message": {
			chunks:  []scriptedChunk{{data: unrelated, err: errWrappedBufferFull}},
			wantErr: errWrappedBufferFull,
		},
		"error: channel error wrapping the buffer-full sentinel in the read after the first message": {
			chunks:  []scriptedChunk{{data: unrelated}, {err: errWrappedBufferFull}},
			wantErr: errWrappedBufferFull,
		},
		"error: the buffer-full sentinel itself as the channel error in the read after the first message": {
			chunks:  []scriptedChunk{{data: unrelated}, {err: bufio.ErrBufferFull}},
			wantErr: bufio.ErrBufferFull,
		},
		"error: channel error with a first message longer than the default buffer": {
			chunks:  []scriptedChunk{{data: longUnrelatedMessage, err: errScriptedChannel}},
			wantErr: errScriptedChannel,
		},
		"success: clean end after a first message longer than the default buffer": {
			chunks: []scriptedChunk{{data: longUnrelatedMessage}},
		},
		"success: clean end in the same read as the first message": {
			chunks: []scriptedChunk{{data: unrelated, err: io.EOF}},
		},
		"success: clean end in the read after the first message": {
			chunks: []scriptedChunk{{data: unrelated}},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			ch := &scriptedChannel{t: t, chunks: tt.chunks}
			stop, err := supervisor.FixtureShutdown(ch, true)

			if stop {
				t.Error("FixtureShutdown reported a shutdown request, want none for an unrelated message")
			}
			if tt.wantErr == nil {
				if err != nil {
					t.Errorf("FixtureShutdown error = %v, want nil at the channel's clean end", err)
				}
			} else {
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("FixtureShutdown error = %v, want the channel's error %v", err, tt.wantErr)
				}
				if errors.Is(err, errReadAfterError) {
					t.Errorf("FixtureShutdown error = %v comes from a read after the channel's error", err)
				}
			}
			if diff := gocmp.Diff(0, ch.readsAfterError); diff != "" {
				t.Errorf("reads after the channel's error (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(1, ch.closes); diff != "" {
				t.Errorf("channel closes (-want +got):\n%s", diff)
			}
		})
	}
}
