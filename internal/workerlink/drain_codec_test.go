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

package workerlink_test

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// frame returns the compact wire form the writer must emit for one message.
func frame(typ, generation string) string {
	return `{"type":"` + typ + `","generation":"` + generation + `"}` + "\n"
}

// fillGeneration returns an ASCII generation whose compact frame of type typ is exactly size bytes long.
func fillGeneration(typ string, size int) string {
	return strings.Repeat("a", size-len(frame(typ, "")))
}

// sentinels are the codec's declared failures; a decision names exactly one of them, or none.
var codecSentinels = []error{workerlink.ErrDrainMessage, workerlink.ErrDrainLineTooLong, io.EOF}

func checkCodecError(t *testing.T, err, want error) {
	t.Helper()
	if want == nil {
		if err != nil {
			t.Fatalf("error = %v, want nil", err)
		}
		return
	}
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
	for _, other := range codecSentinels {
		if !errors.Is(other, want) && errors.Is(err, other) {
			t.Errorf("error %v also matches %v; the failures must stay distinguishable", err, other)
		}
	}
}

func TestEncodeDrain(t *testing.T) {
	limitDraining := fillGeneration(workerlink.Draining, workerlink.DrainLineLimit)
	limitRetired := fillGeneration(workerlink.Retired, workerlink.DrainLineLimit)
	// A quotation mark costs two bytes on the wire, so the limit is a bound on encoded bytes, not on the generation.
	quotesAtLimit := strings.Repeat(`"`, len(limitDraining)/2)
	tests := map[string]struct {
		msg  workerlink.DrainMessage
		want string
		err  error
	}{
		"success: draining": {
			msg:  workerlink.DrainMessage{Type: workerlink.Draining, Generation: "g1"},
			want: frame("draining", "g1"),
		},
		"success: retired": {
			msg:  workerlink.DrainMessage{Type: workerlink.Retired, Generation: "g1"},
			want: frame("retired", "g1"),
		},
		"success: draining frame of exactly the limit": {
			msg:  workerlink.DrainMessage{Type: workerlink.Draining, Generation: workerlink.Generation(limitDraining)},
			want: frame("draining", limitDraining),
		},
		"success: retired frame of exactly the limit": {
			msg:  workerlink.DrainMessage{Type: workerlink.Retired, Generation: workerlink.Generation(limitRetired)},
			want: frame("retired", limitRetired),
		},
		"success: escaped generation filling the limit": {
			msg:  workerlink.DrainMessage{Type: workerlink.Draining, Generation: workerlink.Generation(quotesAtLimit)},
			want: frame("draining", strings.Repeat(`\"`, len(quotesAtLimit))),
		},
		"error: one byte over the limit": {
			msg: workerlink.DrainMessage{Type: workerlink.Draining, Generation: workerlink.Generation(limitDraining + "a")},
			err: workerlink.ErrDrainLineTooLong,
		},
		"error: escapes push the frame over the limit": {
			msg: workerlink.DrainMessage{Type: workerlink.Draining, Generation: workerlink.Generation(quotesAtLimit + `"`)},
			err: workerlink.ErrDrainLineTooLong,
		},
		"error: unknown type": {
			msg: workerlink.DrainMessage{Type: "drained", Generation: "g1"},
			err: workerlink.ErrDrainMessage,
		},
		"error: type in another case": {
			msg: workerlink.DrainMessage{Type: "Draining", Generation: "g1"},
			err: workerlink.ErrDrainMessage,
		},
		"error: empty type": {
			msg: workerlink.DrainMessage{Generation: "g1"},
			err: workerlink.ErrDrainMessage,
		},
		"error: empty generation": {
			msg: workerlink.DrainMessage{Type: workerlink.Retired},
			err: workerlink.ErrDrainMessage,
		},
		"error: generation that is not UTF-8": {
			msg: workerlink.DrainMessage{Type: workerlink.Draining, Generation: "g\xff"},
			err: workerlink.ErrDrainMessage,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var out closeRecorder
			err := workerlink.EncodeDrain(&out, tt.msg)
			checkCodecError(t, err, tt.err)
			if diff := gocmp.Diff(tt.want, out.String()); diff != "" {
				t.Errorf("written bytes (-want +got):\n%s", diff)
			}
			if tt.err == nil && len(out.String()) > workerlink.DrainLineLimit {
				t.Errorf("frame of %d bytes exceeds the %d-byte limit", len(out.String()), workerlink.DrainLineLimit)
			}
			if out.closed {
				t.Error("EncodeDrain closed the writer")
			}
		})
	}
}

// TestEncodeDrainRoundTrip checks that what the writer emits for awkward generations is one frame the reader
// returns unchanged.
func TestEncodeDrainRoundTrip(t *testing.T) {
	tests := map[string]workerlink.DrainMessage{
		"success: line feed and carriage return in the generation": {Type: workerlink.Draining, Generation: "a\nb\rc"},
		"success: quote and backslash in the generation":           {Type: workerlink.Retired, Generation: `a"b\c`},
		"success: markup characters in the generation":             {Type: workerlink.Draining, Generation: "<a&b>"},
		"success: non-ASCII generation":                            {Type: workerlink.Retired, Generation: "世代-1"},
		"success: control and separator characters":                {Type: workerlink.Draining, Generation: "\x00\x1f "},
	}
	for name, msg := range tests {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			if err := workerlink.EncodeDrain(&out, msg); err != nil {
				t.Fatalf("EncodeDrain: %v", err)
			}
			wire := out.String()
			if strings.Count(wire, "\n") != 1 || !strings.HasSuffix(wire, "\n") {
				t.Fatalf("frame %q must hold exactly one LF, at its end", wire)
			}
			if strings.HasPrefix(wire, "\xef\xbb\xbf") || strings.Contains(wire, "\r") {
				t.Fatalf("frame %q carries a byte order mark or a carriage return", wire)
			}
			got, err := workerlink.DecodeDrain(bufio.NewReader(strings.NewReader(wire)))
			if err != nil {
				t.Fatalf("DecodeDrain(%q): %v", wire, err)
			}
			if diff := gocmp.Diff(msg, got); diff != "" {
				t.Errorf("round trip (-want +got):\n%s", diff)
			}
		})
	}
}

// TestEncodeDrainWriterFailures checks that a frame the writer did not take completely is never reported as sent.
func TestEncodeDrainWriterFailures(t *testing.T) {
	errWriter := errors.New("writer failed")
	msg := workerlink.DrainMessage{Type: workerlink.Draining, Generation: "g1"}
	tests := map[string]struct {
		w     io.Writer
		wants []error
	}{
		"error: writer fails": {
			w:     &failingWriter{err: errWriter},
			wants: []error{errWriter},
		},
		"error: writer takes part of the frame without an error": {
			// A later write fails, so a writer that retries the remainder still sees a failure, never success.
			w:     &shortWriter{err: errWriter},
			wants: []error{io.ErrShortWrite, errWriter},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := workerlink.EncodeDrain(tt.w, msg)
			if err == nil {
				t.Fatal("EncodeDrain reported success for an incomplete write")
			}
			matched := false
			for _, want := range tt.wants {
				matched = matched || errors.Is(err, want)
			}
			if !matched {
				t.Errorf("error = %v, want one of %v", err, tt.wants)
			}
		})
	}
}

func TestDecodeDrain(t *testing.T) {
	limitDraining := fillGeneration(workerlink.Draining, workerlink.DrainLineLimit)
	atLimit := frame("draining", limitDraining)
	overLimit := frame("draining", limitDraining+"a")
	unterminatedAtLimit := strings.TrimSuffix(atLimit, "\n") + "}"
	valid := frame("draining", "g1")
	tests := map[string]struct {
		input string
		want  workerlink.DrainMessage
		err   error
	}{
		"success: draining": {
			input: valid,
			want:  workerlink.DrainMessage{Type: workerlink.Draining, Generation: "g1"},
		},
		"success: retired": {
			input: frame("retired", "g1"),
			want:  workerlink.DrainMessage{Type: workerlink.Retired, Generation: "g1"},
		},
		"success: members in the other order": {
			input: `{"generation":"g1","type":"retired"}` + "\n",
			want:  workerlink.DrainMessage{Type: workerlink.Retired, Generation: "g1"},
		},
		"success: JSON whitespace between tokens": {
			input: "{ \"type\" :\t\"draining\" ,  \"generation\":\"g1\" }\n",
			want:  workerlink.DrainMessage{Type: workerlink.Draining, Generation: "g1"},
		},
		"success: JSON whitespace around the object": {
			input: " \t{\"type\":\"draining\",\"generation\":\"g1\"} \t\n",
			want:  workerlink.DrainMessage{Type: workerlink.Draining, Generation: "g1"},
		},
		"success: escapes are not frame delimiters": {
			input: `{"type":"draining","generation":"a\nb\"c\\dé😀"}` + "\n",
			want:  workerlink.DrainMessage{Type: workerlink.Draining, Generation: "a\nb\"c\\dé😀"},
		},
		"success: escaped type name and value": {
			input: `{"type":"retired","generation":"g1"}` + "\n",
			want:  workerlink.DrainMessage{Type: workerlink.Retired, Generation: "g1"},
		},
		"success: raw non-ASCII generation": {
			input: frame("retired", "世代-1"),
			want:  workerlink.DrainMessage{Type: workerlink.Retired, Generation: "世代-1"},
		},
		"success: terminated frame of exactly the limit": {
			input: atLimit,
			want:  workerlink.DrainMessage{Type: workerlink.Draining, Generation: workerlink.Generation(limitDraining)},
		},
		"error: nothing before end of file": {
			input: "",
			err:   io.EOF,
		},
		"error: partial line before end of file": {
			input: strings.TrimSuffix(valid, "\n"),
			err:   workerlink.ErrDrainMessage,
		},
		"error: terminated frame one byte over the limit": {
			input: overLimit,
			err:   workerlink.ErrDrainLineTooLong,
		},
		"error: limit reached without a line feed before more data": {
			input: unterminatedAtLimit + "\n",
			err:   workerlink.ErrDrainLineTooLong,
		},
		"error: limit reached without a line feed before end of file": {
			input: unterminatedAtLimit,
			err:   workerlink.ErrDrainLineTooLong,
		},
		"error: oversized line followed by a valid frame": {
			input: overLimit + valid,
			err:   workerlink.ErrDrainLineTooLong,
		},
		"error: blank line": {
			input: "\n" + valid,
			err:   workerlink.ErrDrainMessage,
		},
		"error: line ended by CRLF": {
			input: strings.TrimSuffix(valid, "\n") + "\r\n",
			err:   workerlink.ErrDrainMessage,
		},
		"error: byte order mark": {
			input: "\xef\xbb\xbf" + valid,
			err:   workerlink.ErrDrainMessage,
		},
		"error: line feed inside the object": {
			input: `{"type":"draining",` + "\n" + `"generation":"g1"}` + "\n",
			err:   workerlink.ErrDrainMessage,
		},
		"error: malformed line followed by a valid frame": {
			input: "draining g1\n" + valid,
			err:   workerlink.ErrDrainMessage,
		},
		"error: unknown member": {
			input: `{"type":"draining","generation":"g1","pid":1}` + "\n",
			err:   workerlink.ErrDrainMessage,
		},
		"error: duplicate member with another value": {
			input: `{"type":"draining","type":"retired","generation":"g1"}` + "\n",
			err:   workerlink.ErrDrainMessage,
		},
		"error: duplicate member with the same value": {
			input: `{"type":"draining","generation":"g1","generation":"g1"}` + "\n",
			err:   workerlink.ErrDrainMessage,
		},
		"error: duplicate member spelled with an escape": {
			input: `{"type":"draining","type":"draining","generation":"g1"}` + "\n",
			err:   workerlink.ErrDrainMessage,
		},
		"error: trailing value": {
			input: strings.TrimSuffix(valid, "\n") + " {}\n",
			err:   workerlink.ErrDrainMessage,
		},
		"error: trailing bytes": {
			input: strings.TrimSuffix(valid, "\n") + "x\n",
			err:   workerlink.ErrDrainMessage,
		},
		"error: empty object": {
			input: "{}\n",
			err:   workerlink.ErrDrainMessage,
		},
		"error: missing type": {
			input: `{"generation":"g1"}` + "\n",
			err:   workerlink.ErrDrainMessage,
		},
		"error: missing generation": {
			input: `{"type":"draining"}` + "\n",
			err:   workerlink.ErrDrainMessage,
		},
		"error: type is not a string": {
			input: `{"type":1,"generation":"g1"}` + "\n",
			err:   workerlink.ErrDrainMessage,
		},
		"error: type is null": {
			input: `{"type":null,"generation":"g1"}` + "\n",
			err:   workerlink.ErrDrainMessage,
		},
		"error: generation is a number": {
			input: `{"type":"draining","generation":1}` + "\n",
			err:   workerlink.ErrDrainMessage,
		},
		"error: generation is null": {
			input: `{"type":"draining","generation":null}` + "\n",
			err:   workerlink.ErrDrainMessage,
		},
		"error: unknown type": {
			input: frame("drained", "g1"),
			err:   workerlink.ErrDrainMessage,
		},
		"error: type in another case": {
			input: frame("Draining", "g1"),
			err:   workerlink.ErrDrainMessage,
		},
		"error: member name in another case": {
			input: `{"Type":"draining","generation":"g1"}` + "\n",
			err:   workerlink.ErrDrainMessage,
		},
		"error: empty generation": {
			input: frame("retired", ""),
			err:   workerlink.ErrDrainMessage,
		},
		"error: generation that is not UTF-8": {
			input: frame("draining", "g\xff"),
			err:   workerlink.ErrDrainMessage,
		},
		"error: lone surrogate escape": {
			input: frame("draining", `g\ud800`),
			err:   workerlink.ErrDrainMessage,
		},
		"error: array": {
			input: `["draining","g1"]` + "\n",
			err:   workerlink.ErrDrainMessage,
		},
		"error: not JSON": {
			input: "draining g1\n",
			err:   workerlink.ErrDrainMessage,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := workerlink.DecodeDrain(bufio.NewReader(strings.NewReader(tt.input)))
			checkCodecError(t, err, tt.err)
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("message (-want +got):\n%s", diff)
			}
		})
	}
}

// TestDecodeDrainStream checks that the reader takes one frame per call from a persistent buffered reader, keeps
// what it has already buffered for the next call, and does not depend on the reader's buffer being as large as
// the line limit.
func TestDecodeDrainStream(t *testing.T) {
	long := fillGeneration(workerlink.Retired, workerlink.DrainLineLimit)
	tests := map[string]struct {
		input string
		size  int
		want  []workerlink.DrainMessage
		final error
	}{
		"success: two frames buffered together": {
			input: frame("draining", "g1") + frame("retired", "g1"),
			size:  4096,
			want: []workerlink.DrainMessage{
				{Type: workerlink.Draining, Generation: "g1"},
				{Type: workerlink.Retired, Generation: "g1"},
			},
			final: io.EOF,
		},
		"success: frames longer than the reader's buffer": {
			input: frame("retired", long) + frame("draining", "g2"),
			size:  16,
			want: []workerlink.DrainMessage{
				{Type: workerlink.Retired, Generation: workerlink.Generation(long)},
				{Type: workerlink.Draining, Generation: "g2"},
			},
			final: io.EOF,
		},
		"error: oversized line through a small buffer": {
			input: frame("retired", long+"a"),
			size:  16,
			final: workerlink.ErrDrainLineTooLong,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			r := bufio.NewReaderSize(strings.NewReader(tt.input), tt.size)
			for i, want := range tt.want {
				got, err := workerlink.DecodeDrain(r)
				if err != nil {
					t.Fatalf("frame %d: %v", i, err)
				}
				if diff := gocmp.Diff(want, got); diff != "" {
					t.Fatalf("frame %d (-want +got):\n%s", i, diff)
				}
			}
			got, err := workerlink.DecodeDrain(r)
			checkCodecError(t, err, tt.final)
			if diff := gocmp.Diff(workerlink.DrainMessage{}, got); diff != "" {
				t.Errorf("message after the last frame (-want +got):\n%s", diff)
			}
		})
	}
}

// TestDecodeDrainLiveChannel checks that the reader answers as soon as a frame or an excess is complete on a
// channel that stays open; it never waits for end of file. The watchdog only bounds a hang.
func TestDecodeDrainLiveChannel(t *testing.T) {
	tests := map[string]struct {
		write string
		want  workerlink.DrainMessage
		err   error
	}{
		"success: frame on an open channel": {
			write: frame("retired", "g1"),
			want:  workerlink.DrainMessage{Type: workerlink.Retired, Generation: "g1"},
		},
		"error: excess on an open channel": {
			write: strings.Repeat("x", workerlink.DrainLineLimit+44),
			err:   workerlink.ErrDrainLineTooLong,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			pr, pw := io.Pipe()
			wrote := make(chan struct{})
			go func() {
				defer close(wrote)
				_, _ = pw.Write([]byte(tt.write))
			}()
			t.Cleanup(func() {
				_ = pw.CloseWithError(errors.New("test finished"))
				_ = pr.Close()
				<-wrote
			})
			type result struct {
				msg workerlink.DrainMessage
				err error
			}
			done := make(chan result, 1)
			go func() {
				msg, err := workerlink.DecodeDrain(bufio.NewReader(pr))
				done <- result{msg, err}
			}()
			var res result
			select {
			case res = <-done:
			case <-time.After(10 * time.Second):
				_ = pw.CloseWithError(errors.New("watchdog"))
				res = <-done
				t.Fatalf("DecodeDrain did not answer on an open channel (after the watchdog: %+v)", res)
			}
			checkCodecError(t, res.err, tt.err)
			if diff := gocmp.Diff(tt.want, res.msg); diff != "" {
				t.Errorf("message (-want +got):\n%s", diff)
			}
		})
	}
}

// TestDecodeDrainReadFailure checks that a failure of the channel itself stays distinguishable from a bad frame.
func TestDecodeDrainReadFailure(t *testing.T) {
	errRead := errors.New("channel failed")
	tests := map[string]string{
		"error: failure before any byte":          "",
		"error: failure in the middle of a frame": `{"type":"drain`,
	}
	for name, prefix := range tests {
		t.Run(name, func(t *testing.T) {
			r := bufio.NewReaderSize(io.MultiReader(strings.NewReader(prefix), &failingReader{err: errRead}), 16)
			got, err := workerlink.DecodeDrain(r)
			if !errors.Is(err, errRead) {
				t.Fatalf("error = %v, want %v", err, errRead)
			}
			for _, other := range codecSentinels {
				if errors.Is(err, other) {
					t.Errorf("error %v matches %v; a channel failure must stay distinguishable", err, other)
				}
			}
			if diff := gocmp.Diff(workerlink.DrainMessage{}, got); diff != "" {
				t.Errorf("message (-want +got):\n%s", diff)
			}
		})
	}
}

// closeRecorder is a writer that remembers whether it was closed.
type closeRecorder struct {
	bytes.Buffer
	closed bool
}

func (w *closeRecorder) Close() error {
	w.closed = true
	return nil
}

type failingWriter struct{ err error }

func (w *failingWriter) Write([]byte) (int, error) { return 0, w.err }

// shortWriter takes half of its first write without reporting an error, then fails every later write.
type shortWriter struct {
	err   error
	calls int
}

func (w *shortWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls == 1 {
		return len(p) / 2, nil
	}
	return 0, w.err
}

type failingReader struct{ err error }

func (r *failingReader) Read([]byte) (int, error) { return 0, r.err }
