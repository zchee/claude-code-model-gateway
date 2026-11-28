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
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/supervisor"
)

const (
	// hbLimit is the byte budget of one request head on the public listener: net/http's default header limit plus
	// its read slack, the same budget as for a trailer section. A head of exactly hbLimit bytes, its closing empty
	// line included, fits.
	hbLimit = http.DefaultMaxHeaderBytes + 4096
	// hbServerBuffer is the size of the buffer the public listener reads a connection through; it bounds how many
	// bytes of a head the server can already hold when it begins to read the head.
	hbServerBuffer = 4096
	// hbRefusal is the listener's whole answer to a head over the budget; the connection ends after it.
	hbRefusal = "HTTP/1.1 431 Request Header Fields Too Large\r\nConnection: close\r\nContent-Length: 0\r\n\r\n"
	// hbGuard turns a server that neither answers nor closes into a failure; it is never a pacing delay.
	hbGuard = 30 * time.Second
	// hbHost is the Host of every request; the test's handler does not judge it, and a fixed value keeps the
	// measured head byte for byte the same on every connection.
	hbHost = "head-budget.test"
	// hbBefore is the request sent ahead of the measured one when the measured one is a later request, and hbAfter
	// the request sent after it on every connection; it asks for the connection's end, so that an accepted head ends
	// the connection by HTTP's own rule. hbBody is the measured request's body.
	hbBefore = "POST /before HTTP/1.1\r\nHost: " + hbHost + "\r\nContent-Length: 2\r\n\r\n{}"
	hbAfter  = "GET /after HTTP/1.1\r\nHost: " + hbHost + "\r\nConnection: close\r\n\r\n"
	hbBody   = "{}"
)

// hbHead returns the measured request head of exactly size bytes, its closing empty line included. It announces a
// two-byte body, so that the body's bytes, and those of the request after it, follow the head's last bytes.
func hbHead(size int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "POST /measured HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n", hbHost, len(hbBody))
	const longLine, shortest = 4000, len("X-Pad0000: p\r\n")
	for i := 0; ; i++ {
		remain := size - len("\r\n") - b.Len()
		if remain <= 0 {
			break
		}
		n := min(longLine, remain)
		if rest := remain - n; rest > 0 && rest < shortest {
			n = remain - shortest
		}
		name := fmt.Sprintf("X-Pad%04d: ", i)
		b.WriteString(name)
		b.WriteString(strings.Repeat("p", n-len(name)-len("\r\n")))
		b.WriteString("\r\n")
	}
	b.WriteString("\r\n")
	return b.String()
}

// hbAddr is the address both ends of a staged connection report.
var hbAddr = &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9}

// hbStagedConn is a connection whose whole client stream is staged before the server reads it, so that the server's
// reads are exactly the windows the test prescribes: a read never crosses split, a positive limit caps every read,
// and otherwise a read returns as much as the caller asks for. As on a TCP connection, a read after its deadline has
// passed fails with a timeout even when data is there. Once the stream is read the client's write side stays open: a
// read waits for its deadline or the connection's close. Everything the server writes is kept.
type hbStagedConn struct {
	split int
	limit int

	mu       sync.Mutex
	in       []byte
	off      int
	ends     []int // The stream offset at which each read that returned bytes ended.
	out      bytes.Buffer
	deadline time.Time
	closed   bool
	changed  chan struct{}
	done     chan struct{}
	// wrote is signalled after every write of the server.
	wrote chan struct{}
}

func newHBStagedConn(stream string, split, limit int) *hbStagedConn {
	return &hbStagedConn{in: []byte(stream), split: split, limit: limit, changed: make(chan struct{}), done: make(chan struct{}), wrote: make(chan struct{}, 1)}
}

// feed appends data to the client's stream, as a client that writes more on the open connection.
func (c *hbStagedConn) feed(data string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.in = append(c.in, data...)
	close(c.changed)
	c.changed = make(chan struct{})
}

func (c *hbStagedConn) Read(p []byte) (int, error) {
	for {
		c.mu.Lock()
		switch {
		case c.closed:
			c.mu.Unlock()
			return 0, net.ErrClosed
		case len(p) == 0:
			c.mu.Unlock()
			return 0, nil
		case !c.deadline.IsZero() && !time.Now().Before(c.deadline):
			// As a TCP connection's read, an expired deadline wins over data that is already there.
			c.mu.Unlock()
			return 0, &net.OpError{Op: "read", Net: "tcp", Source: hbAddr, Addr: hbAddr, Err: os.ErrDeadlineExceeded}
		case c.off < len(c.in):
			n := min(len(p), len(c.in)-c.off)
			if c.off < c.split {
				n = min(n, c.split-c.off)
			}
			if c.limit > 0 {
				n = min(n, c.limit)
			}
			copy(p, c.in[c.off:c.off+n])
			c.off += n
			c.ends = append(c.ends, c.off)
			c.mu.Unlock()
			return n, nil
		}
		d, changed := c.deadline, c.changed
		c.mu.Unlock()
		if d.IsZero() {
			<-changed
			continue
		}
		timer := time.NewTimer(time.Until(d))
		select {
		case <-changed:
		case <-timer.C:
		}
		timer.Stop()
	}
}

func (c *hbStagedConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, net.ErrClosed
	}
	defer func() {
		select {
		case c.wrote <- struct{}{}:
		default:
		}
	}()
	return c.out.Write(p)
}

func (c *hbStagedConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		close(c.done)
		close(c.changed)
		c.changed = make(chan struct{})
	}
	return nil
}

// CloseWrite is the half-close the server uses before it closes a connection; the staged client keeps reading.
func (c *hbStagedConn) CloseWrite() error { return nil }

func (c *hbStagedConn) LocalAddr() net.Addr  { return hbAddr }
func (c *hbStagedConn) RemoteAddr() net.Addr { return hbAddr }

func (c *hbStagedConn) SetDeadline(d time.Time) error { return c.SetReadDeadline(d) }

func (c *hbStagedConn) SetReadDeadline(d time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadline = d
	close(c.changed)
	c.changed = make(chan struct{})
	return nil
}

func (*hbStagedConn) SetWriteDeadline(time.Time) error { return nil }

// written returns what the server wrote and the stream offsets at which its reads ended.
func (c *hbStagedConn) written() ([]byte, []int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.Clone(c.out.Bytes()), slices.Clone(c.ends)
}

// hbStagedListener accepts its one staged connection and then waits for its own close.
type hbStagedListener struct {
	conn   *hbStagedConn
	once   sync.Once
	closed chan struct{}
	stop   func()
}

func newHBStagedListener(conn *hbStagedConn) *hbStagedListener {
	l := &hbStagedListener{conn: conn, closed: make(chan struct{})}
	l.stop = sync.OnceFunc(func() { close(l.closed) })
	return l
}

func (l *hbStagedListener) Accept() (net.Conn, error) {
	var conn net.Conn
	l.once.Do(func() { conn = l.conn })
	if conn != nil {
		return conn, nil
	}
	<-l.closed
	return nil, net.ErrClosed
}

func (l *hbStagedListener) Close() error   { l.stop(); return nil }
func (l *hbStagedListener) Addr() net.Addr { return hbAddr }

// hbAnswer is one answer the client parsed.
type hbAnswer struct {
	Status int
	Close  bool
}

// hbVerdict is what one connection observed: every answer in order, the requests the handler served, whether the
// connection ended with exactly the listener's refusal, whether the server ended the connection at all, and any
// bytes that are not a complete answer.
type hbVerdict struct {
	Answers []hbAnswer
	Served  []string
	Refused bool
	Ended   bool
	Problem string
}

func (v hbVerdict) String() string {
	return fmt.Sprintf("answers %v, served %v, refused %t, ended %t, problem %q", v.Answers, v.Served, v.Refused, v.Ended, v.Problem)
}

// hbSend serves stream, staged with the given read windows, on a fresh public listener and returns what the client
// and the handler observed, with the stream offsets at which the server's reads ended.
func hbSend(t *testing.T, stream string, split, limit int) (hbVerdict, []int) {
	t.Helper()
	var mu sync.Mutex
	var served []string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			return
		}
		mu.Lock()
		served = append(served, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Length", "2")
		_, _ = io.WriteString(w, "ok")
	})
	conn := newHBStagedConn(stream, split, limit)
	ln := newHBStagedListener(conn)
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	done := make(chan error, 1)
	go func() { done <- supervisor.Serve(ctx, ln, handler) }()
	var v hbVerdict
	timer := time.NewTimer(hbGuard)
	select {
	case <-conn.done:
		v.Ended = true
	case <-timer.C:
		// The server did not end the connection; end it here, so that the listener's join below can complete.
		_ = conn.Close()
	}
	timer.Stop()
	cancel()
	join := time.NewTimer(hbGuard)
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("public listener: %v", err)
		}
	case <-join.C:
		t.Errorf("the public listener did not return within %v of its cancellation, with its connection closed", hbGuard)
	}
	join.Stop()
	raw, ends := conn.written()
	br := bufio.NewReader(bytes.NewReader(raw))
	for {
		if _, err := br.Peek(1); err != nil {
			break
		}
		if start, _ := br.Peek(len("HTTP/1.1 431 ")); string(start) == "HTTP/1.1 431 " {
			rest, _ := io.ReadAll(br)
			v.Refused = string(rest) == hbRefusal
			if !v.Refused {
				v.Problem = "refusal bytes " + strconv.Quote(string(rest))
			}
			v.Answers = append(v.Answers, hbAnswer{Status: http.StatusRequestHeaderFieldsTooLarge, Close: true})
			break
		}
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			rest, _ := io.ReadAll(br)
			v.Problem = fmt.Sprintf("unparsable answer (%v): %q", err, rest)
			break
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || string(body) != "ok" {
			v.Problem = fmt.Sprintf("answer body %q: %v", body, err)
			break
		}
		v.Answers = append(v.Answers, hbAnswer{Status: resp.StatusCode, Close: resp.Close})
	}
	mu.Lock()
	v.Served = slices.Clone(served)
	mu.Unlock()
	return v, ends
}

// TestRequestHeadBudget checks that identical request heads get the same verdict at the byte limit, one byte below it
// and one byte above it, whether the head is the first request of its connection or a later one, and however the
// server's reads split its bytes. The budget charges every byte of the head, including bytes the server had already
// read before it began to read the head, and none of the bytes of the body or of the next request that come with the
// head's end. Each connection's whole stream is staged before the server reads it, so each read window is exactly the
// one prescribed: as much as the server asks for, at most 509 bytes, or a first window ending at a chosen offset into
// the head, up to the end of the server's buffer. The client never ends its side; an accepted head's connection ends
// because the last request asks for it, a refused one's because of the refusal.
func TestRequestHeadBudget(t *testing.T) {
	accepted := func(later bool) hbVerdict {
		if later {
			return hbVerdict{
				Answers: []hbAnswer{{Status: http.StatusOK}, {Status: http.StatusOK}, {Status: http.StatusOK, Close: true}},
				Served:  []string{"/before", "/measured", "/after"}, Ended: true,
			}
		}
		return hbVerdict{Answers: []hbAnswer{{Status: http.StatusOK}, {Status: http.StatusOK, Close: true}}, Served: []string{"/measured", "/after"}, Ended: true}
	}
	refused := func(later bool) hbVerdict {
		if later {
			return hbVerdict{
				Answers: []hbAnswer{{Status: http.StatusOK}, {Status: http.StatusRequestHeaderFieldsTooLarge, Close: true}},
				Served:  []string{"/before"}, Refused: true, Ended: true,
			}
		}
		return hbVerdict{Answers: []hbAnswer{{Status: http.StatusRequestHeaderFieldsTooLarge, Close: true}}, Refused: true, Ended: true}
	}
	tests := map[string]struct {
		size  int
		later bool
		want  hbVerdict
	}{
		"success: first request with a head one byte below the limit": {size: hbLimit - 1, want: accepted(false)},
		"success: first request with a head of exactly the limit":     {size: hbLimit, want: accepted(false)},
		"error: first request with a head one byte above the limit":   {size: hbLimit + 1, want: refused(false)},
		"success: later request with a head one byte below the limit": {size: hbLimit - 1, later: true, want: accepted(true)},
		"success: later request with a head of exactly the limit":     {size: hbLimit, later: true, want: accepted(true)},
		"error: later request with a head one byte above the limit":   {size: hbLimit + 1, later: true, want: refused(true)},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			head := hbHead(tc.size)
			if len(head) != tc.size {
				t.Fatalf("built a head of %d bytes; want %d", len(head), tc.size)
			}
			before := ""
			if tc.later {
				before = hbBefore
			}
			stream := before + head + hbBody + hbAfter
			headStart := len(before)
			// The offsets into the head at which the server's first window ends: the head's first bytes, a few within
			// its lines, and the last bytes the server's buffer can hold together with what came before the head.
			last := hbServerBuffer - len(before)
			type segmentation struct {
				label        string
				split, limit int
			}
			segmentations := []segmentation{{label: "reads as large as the server asks"}, {label: "reads of at most 509 bytes", limit: 509}}
			for _, k := range []int{1, 2, 3, 61, 1021, last - 2, last - 1, last} {
				segmentations = append(segmentations, segmentation{label: "first window ending " + strconv.Itoa(k) + " bytes into the head", split: headStart + k})
			}
			verdicts := make(map[string][]string)
			byText := make(map[string]hbVerdict)
			for _, s := range segmentations {
				got, ends := hbSend(t, stream, s.split, s.limit)
				verdicts[got.String()] = append(verdicts[got.String()], s.label)
				byText[got.String()] = got
				// The windows the server's reads really had: a split row must have a read that ended at its offset,
				// and a capped row no read longer than its cap.
				switch {
				case s.split > 0 && !slices.Contains(ends, s.split):
					t.Errorf("%s: no read ended at stream offset %d; reads ended at %v", s.label, s.split, ends)
				case s.limit > 0:
					prev := 0
					for _, e := range ends {
						if e-prev > s.limit {
							t.Errorf("%s: a read took %d bytes (offsets %d to %d)", s.label, e-prev, prev, e)
						}
						prev = e
					}
				}
			}
			if len(verdicts) != 1 {
				var groups []string
				for v, labels := range verdicts {
					groups = append(groups, fmt.Sprintf("%s: %d segmentation(s), first %q", v, len(labels), labels[0]))
				}
				slices.Sort(groups)
				t.Errorf("a %d-byte head got %d different verdicts depending on how its bytes were read; want one:\n  %s", tc.size, len(verdicts), strings.Join(groups, "\n  "))
			}
			for text, labels := range verdicts {
				if diff := gocmp.Diff(tc.want, byText[text]); diff != "" {
					t.Errorf("verdict on a %d-byte head for %d segmentation(s), first %q (-want +got):\n%s", tc.size, len(labels), labels[0], diff)
				}
			}
		})
	}
}

// TestRequestHeadBudgetOnLoopback sends a first request whose head is one byte over the limit on a real loopback
// connection, followed by its body and a later request, and never ends its side. The client must read exactly the
// refusal and then the connection's orderly end, with the bytes the server did not read still unsent or read away
// by the listener rather than turned into a reset; the handler serves nothing.
func TestRequestHeadBudgetOnLoopback(t *testing.T) {
	type loopback struct {
		Received string
		End      string
		Served   []string
	}
	tests := map[string]struct {
		size int
		want loopback
	}{
		"error: a first request one byte over the limit on a real connection gets exactly the refusal and then the end": {
			size: hbLimit + 1, want: loopback{Received: hbRefusal, End: hcOrderly},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var served []string
			addr, _ := rdServe(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				mu.Lock()
				served = append(served, r.URL.Path)
				mu.Unlock()
			})
			conn := rdDial(t, addr)
			// The target has the same length as the measured head's, so the head keeps its size.
			stream := strings.Replace(hbHead(tc.size), "/measured", "/first?ab", 1) + hbBody + hbAfter
			if len(stream) != tc.size+len(hbBody)+len(hbAfter) {
				t.Fatalf("built a stream of %d bytes; want a head of %d", len(stream), tc.size)
			}
			// Both directions are bounded before the writer starts, so that a listener that stops reading cannot park
			// the writer on backpressure.
			if err := conn.SetDeadline(time.Now().Add(hbGuard)); err != nil {
				t.Fatal(err)
			}
			written := make(chan struct{})
			go func() {
				defer close(written)
				// A write the server cut short by its refusal is an expected result.
				_, _ = io.WriteString(conn, stream)
			}()
			raw, err := io.ReadAll(conn)
			// Closed before the writer is joined, so that a writer still blocked ends.
			_ = conn.Close()
			<-written
			mu.Lock()
			got := loopback{Received: string(raw), End: hcEndKind(err), Served: slices.Clone(served)}
			mu.Unlock()
			if diff := gocmp.Diff(tc.want, got); diff != "" {
				t.Errorf("over-limit head on a loopback connection (-want +got):\n%s", diff)
			}
		})
	}
}
