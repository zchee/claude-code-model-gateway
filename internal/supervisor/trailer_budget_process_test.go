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

//go:build process

package supervisor_test

import (
	"bufio"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
	"github.com/zchee/claude-code-model-gateway/internal/supervisor"
)

// sectionLimit is the byte budget of one trailer section, the same as for a request head: net/http's default header
// limit plus its read slack.
const sectionLimit = http.DefaultMaxHeaderBytes + 4096

// serverBuffer is the size of the buffer the public listener reads a connection through; it bounds how many bytes of
// a trailer section the server can already hold when it begins to read the section.
const serverBuffer = 4096

// segmentListener hands the public listener connections whose reads end exactly at a chosen offset of the stream: no
// read crosses split, so the server's first reads end there. A positive limit caps every read at that many bytes.
type segmentListener struct {
	net.Listener
	split int
	limit int
}

func (l segmentListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return conn, err
	}
	return &segmentConn{Conn: conn, split: l.split, limit: l.limit}, nil
}

type segmentConn struct {
	net.Conn
	split int
	limit int
	mu    sync.Mutex
	read  int
}

func (c *segmentConn) Read(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.read < c.split {
		b = b[:min(len(b), c.split-c.read)]
	}
	if c.limit > 0 {
		b = b[:min(len(b), c.limit)]
	}
	n, err := c.Conn.Read(b)
	c.read += n
	return n, err
}

// CloseWrite keeps the half-close the server uses before it closes a connection.
func (c *segmentConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// trailerSection returns a valid trailer section of exactly size bytes, its closing empty line included. Short field
// lines come first, so that the bytes the server may hold when it begins the section end at many different places
// within a line; long lines make up the rest, well within the limit on field lines.
func trailerSection(size int) string {
	var b strings.Builder
	for i := 0; b.Len() < serverBuffer; i++ {
		fmt.Fprintf(&b, "X-S%03d: v\r\n", i)
	}
	const longLine, shortest = 4000, len("X-L000: \r\n")
	for i := 0; ; i++ {
		remain := size - len("\r\n") - b.Len()
		if remain == 0 {
			break
		}
		n := min(longLine, remain)
		if rest := remain - n; rest > 0 && rest < shortest {
			n = remain - shortest
		}
		name := fmt.Sprintf("X-L%03d: ", i)
		b.WriteString(name + strings.Repeat("p", n-len(name)-len("\r\n")) + "\r\n")
	}
	b.WriteString("\r\n")
	return b.String()
}

// trailerAnswer is what the client and the worker observed for one request.
type trailerAnswer struct {
	Outcome  outcome
	Status   int
	Received int
}

func (a trailerAnswer) String() string {
	return fmt.Sprintf("%s, status %d, received by the worker %d time(s)", a.Outcome, a.Status, a.Received)
}

// sendTrailer writes one chunked request whose trailer section is section to a fresh public listener, with the given
// segmentation of the server's reads, and returns what was observed. at is the number of bytes of the section in the
// server's first read, or -1 to leave the first read unchanged.
func sendTrailer(t *testing.T, worker *recordingWorker, section string, at, limit int) trailerAnswer {
	t.Helper()
	var cfg net.ListenConfig
	ln, err := cfg.Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	own := ln.Addr().String()
	nonce := rand.Text()
	probe := nonce + "-0"
	head := "POST /trailer HTTP/1.1\r\nHost: " + own + "\r\nX-Probe: " + probe + "\r\nTransfer-Encoding: chunked\r\n\r\n"
	const chunks = "2\r\n{}\r\n0\r\n"
	split := 0
	if at >= 0 {
		split = len(head) + len(chunks) + at
	}
	h, err := supervisor.NewHandler(ln.Addr().(*net.TCPAddr).Port, func() int { return worker.port }, strings.Repeat("3d", 32), "1.2.3", 41)
	if err != nil {
		_ = ln.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- supervisor.Serve(ctx, segmentListener{Listener: ln, split: split, limit: limit}, h) }()
	defer func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("public listener: %v", err)
		}
		_ = ln.Close()
	}()

	var dialer net.Dialer
	conn, err := dialer.DialContext(t.Context(), "tcp4", own)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	// A hang guard for a server that neither answers nor closes, not a pacing delay.
	if err := conn.SetDeadline(time.Now().Add(hangGuard)); err != nil {
		t.Fatal(err)
	}
	written := make(chan error, 1)
	go func() {
		_, err := io.WriteString(conn, head+chunks+section)
		written <- err
	}()
	o, status := readOutcome(bufio.NewReader(conn), head, probe)
	_ = conn.Close()
	// A write that fails because the server closed the connection is an expected result.
	<-written
	return trailerAnswer{Outcome: o, Status: status, Received: worker.seen(nonce)[probe]}
}

// TestTrailerSectionBudget checks that a trailer section at the byte limit gets the same answer however the network
// splits it. The budget charges every byte of the section, including bytes the server had already read with the end
// of the body, so the answer does not depend on how many of them there were. Each section is sent with whole reads,
// with reads of one byte, and split at each offset the server can already hold when the section begins: from the
// section's first byte to the end of the server's buffer. A split further into the section cannot change what the
// server holds at that moment. Offsets are taken one by one at both ends of that range and at a prime stride between,
// which crosses the short field lines at every position within a line.
func TestTrailerSectionBudget(t *testing.T) {
	env := gatewaytest.NewEnv(t.TempDir(), nil, nil, nil)
	worker := startRecordingWorker(t, env)
	relayed := trailerAnswer{Outcome: outRelayed, Status: http.StatusTeapot, Received: 1}
	tests := map[string]struct {
		size int
		want trailerAnswer
	}{
		"success: trailer section of exactly the byte limit is relayed": {size: sectionLimit, want: relayed},
		"error: trailer section one byte over the byte limit is refused": {
			size: sectionLimit + 1,
			want: trailerAnswer{Outcome: outRefused, Status: http.StatusBadRequest},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			// Each case sends a few hundred sections of a megabyte; the cases need not wait in turn.
			t.Parallel()
			section := trailerSection(tc.size)
			if len(section) != tc.size {
				t.Fatalf("built a trailer section of %d bytes; want %d", len(section), tc.size)
			}
			// The bytes before the section: the longest head this test writes, and the chunks.
			before := len("POST /trailer HTTP/1.1\r\nHost: 127.0.0.1:65535\r\nX-Probe: \r\nTransfer-Encoding: chunked\r\n\r\n") + len(rand.Text()+"-0") + len("2\r\n{}\r\n0\r\n")
			last := serverBuffer - before
			var offsets []int
			for k := 0; k <= last; k++ {
				if k < 64 || k > last-64 || k%61 == 0 {
					offsets = append(offsets, k)
				}
			}
			type segmentation struct {
				label     string
				at, limit int
			}
			segmentations := []segmentation{{label: "whole reads", at: -1}, {label: "reads of at most 1 bytes", at: -1, limit: 1}}
			for _, k := range offsets {
				segmentations = append(segmentations, segmentation{label: "section split after " + strconv.Itoa(k) + " bytes", at: k})
			}
			// answers groups the segmentations by the answer they got.
			answers := make(map[trailerAnswer][]string)
			for _, s := range segmentations {
				got := sendTrailer(t, worker, section, s.at, s.limit)
				answers[got] = append(answers[got], s.label)
			}
			if len(answers) != 1 {
				var groups []string
				for a, labels := range answers {
					groups = append(groups, fmt.Sprintf("%s: %d segmentation(s), first %q", a, len(labels), labels[0]))
				}
				slices.Sort(groups)
				t.Errorf("a %d-byte trailer section got %d different answers depending on how its bytes were read; want one:\n  %s", tc.size, len(answers), strings.Join(groups, "\n  "))
			}
			for a, labels := range answers {
				if diff := gocmp.Diff(tc.want, a); diff != "" {
					t.Errorf("answer to a %d-byte trailer section for %d segmentation(s), first %q (-want +got):\n%s", tc.size, len(labels), labels[0], diff)
				}
			}
		})
	}
}
