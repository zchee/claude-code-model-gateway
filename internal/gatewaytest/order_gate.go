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

package gatewaytest

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
)

// maxPostHead bounds the request line and headers OrderGate reads byte by byte.
const maxPostHead = 64 << 10

// OrderGate is a loopback collector for an exporter that posts one record per request, served
// over in-memory pipes inside a synctest bubble, that decides when each post reaches it. A new
// connection is parked unread, so the post's sender stays blocked writing it, until every
// goroutine of the bubble is blocked. Then the collector takes every parked post. One parked post
// is recorded as it is. Several parked posts were outstanding together: nothing their sender did
// fixes the order in which independent connections reach a collector, so the collector records
// them in the reverse of their submission order, which a network is free to do. A sender that
// starts a post only after the previous one is on its connection (on a pipe, read by the
// collector) has its posts recorded in submission order, whatever the scheduler does, and a sender
// with two posts outstanding at once never does. These are checks of the order in which a sender
// writes, not of the order in which a TCP receiver reads independent connections.
type OrderGate struct {
	key   func([]byte) (string, error)
	reply string
	// rank is the submission index of each record key the test submits.
	rank map[string]int
	// dialed is signalled, without blocking, whenever a connection is parked.
	dialed chan struct{}

	mu     sync.Mutex // guards parked
	parked []net.Conn

	// The fields below are used only by the bubble's test goroutine.
	keys   []string // record keys in the order the collector took the posts
	bodies [][]byte // the posts' bodies, in the same order
	errs   []error
}

// NewOrderGate returns a collector for the record keys in want, in submission order. key names
// the record a post's body holds and must return an error, not fail a test, for a body it cannot
// name; reply is the whole HTTP response the collector writes to each post.
func NewOrderGate(want []string, key func([]byte) (string, error), reply string) *OrderGate {
	g := &OrderGate{key: key, reply: reply, rank: make(map[string]int, len(want)), dialed: make(chan struct{}, 1)}
	for i, k := range want {
		g.rank[k] = i
	}
	return g
}

// Dial hands the sender one side of a new pipe and parks the other side unread. It has the
// signature of a transport's DialContext.
func (g *OrderGate) Dial(context.Context, string, string) (net.Conn, error) {
	peer, client := net.Pipe()
	g.mu.Lock()
	g.parked = append(g.parked, peer)
	g.mu.Unlock()
	select {
	case g.dialed <- struct{}{}:
	default:
	}
	return client, nil
}

// Keys returns the record keys in the order the collector took the posts.
func (g *OrderGate) Keys() []string { return slices.Clone(g.keys) }

// Bodies returns the posts' bodies in the order the collector took them.
func (g *OrderGate) Bodies() [][]byte { return slices.Clone(g.bodies) }

// gatePost is one post the collector has read and not yet answered.
type gatePost struct {
	conn net.Conn
	key  string
	body []byte
	rank int
}

func (g *OrderGate) takeParked() []net.Conn {
	g.mu.Lock()
	defer g.mu.Unlock()
	batch := g.parked
	g.parked = nil
	return batch
}

func (g *OrderGate) repark(conns []net.Conn) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.parked = append(conns, g.parked...)
}

// take reads, records and answers every parked post, and reports whether there was any. It must
// run only when every other goroutine of the bubble is blocked, so that the parked set is every
// post outstanding at that point.
func (g *OrderGate) take() bool {
	batch := g.takeParked()
	posts := make([]gatePost, 0, len(batch))
	for _, conn := range batch {
		req, err := http.ReadRequest(bufio.NewReader(conn))
		var body []byte
		if err == nil {
			body, err = io.ReadAll(req.Body)
		}
		var post gatePost
		if err == nil {
			post, err = g.named(conn, body)
		}
		if err != nil {
			g.errs = append(g.errs, fmt.Errorf("read post: %w", err))
			_ = conn.Close()
			continue
		}
		posts = append(posts, post)
	}
	slices.SortFunc(posts, func(a, b gatePost) int { return cmp.Compare(b.rank, a.rank) })
	for _, post := range posts {
		g.record(post)
	}
	return len(batch) > 0
}

func (g *OrderGate) named(conn net.Conn, body []byte) (gatePost, error) {
	key, err := g.key(body)
	if err != nil {
		return gatePost{}, err
	}
	rank, ok := g.rank[key]
	if !ok {
		return gatePost{}, fmt.Errorf("collector received %q, which the test did not submit", key)
	}
	return gatePost{conn: conn, key: key, body: body, rank: rank}, nil
}

// record keeps a post and answers it. A write on the pipe returns once the sender has read all
// of it.
func (g *OrderGate) record(post gatePost) {
	g.keys = append(g.keys, post.key)
	g.bodies = append(g.bodies, post.body)
	if _, err := io.WriteString(post.conn, g.reply); err != nil {
		g.errs = append(g.errs, fmt.Errorf("answer %s: %w", post.key, err))
	}
	_ = post.conn.Close()
}

// HandOff decides when the sender starts its second post, which must already be submitted after
// the first, and returns what it found wrong. The first post must be the only one parked; while
// its last body byte is still unread no second post may start, because the first is not yet on
// its connection; once that byte is read, the second post must start and its whole request,
// body included, must arrive although the first post's reply is still withheld, because a sender
// waits for no reply. The first request is read byte by byte up to the end of its headers and then
// to one byte short of its length, so no buffer reads ahead of what the test means to consume; the
// second is read by its own goroutine while the reply is withheld. HandOff then records and answers
// the first post, then the second, and leaves every other connection parked for Serve. It must run
// inside the bubble, from its test goroutine.
func (g *OrderGate) HandOff() []string {
	var findings []string
	synctest.Wait()
	batch := g.takeParked()
	if len(batch) != 1 {
		g.repark(batch)
		return append(findings, fmt.Sprintf("%d posts were outstanding when the bubble first went quiet, want only the first", len(batch)))
	}
	first := batch[0]
	head, err := readHead(first)
	var length int64
	if err == nil {
		var req *http.Request
		if req, err = http.ReadRequest(bufio.NewReader(bytes.NewReader(head))); err == nil {
			if length = req.ContentLength; length <= 0 {
				err = fmt.Errorf("the first post has content length %d, want a known positive length", length)
			}
		}
	}
	body := make([]byte, max(length, 1))
	if err == nil {
		_, err = io.ReadFull(first, body[:length-1])
	}
	if err != nil {
		g.errs = append(g.errs, fmt.Errorf("read the first post: %w", err))
		_ = first.Close()
		return findings
	}

	synctest.Wait()
	if early := g.takeParked(); len(early) > 0 {
		findings = append(findings, fmt.Sprintf("%d post(s) started while the first post's last body byte was still unwritten", len(early)))
		g.repark(early)
	}
	if _, err := io.ReadFull(first, body[length-1:length]); err != nil {
		g.errs = append(g.errs, fmt.Errorf("read the first post's last byte: %w", err))
		_ = first.Close()
		return findings
	}

	synctest.Wait()
	next := g.takeParked()
	if len(next) == 0 {
		findings = append(findings, "no next post started once the first post was written, while its reply was withheld")
	}
	// The second request is read by its own goroutine, so that a sender that holds it back until
	// the first reply leaves the test goroutine free to find that out and release the reply.
	type read struct {
		body []byte
		err  error
	}
	var second chan read
	if len(next) > 0 {
		g.repark(next[1:])
		second = make(chan read, 1)
		go func() {
			req, err := http.ReadRequest(bufio.NewReader(next[0]))
			var body []byte
			if err == nil {
				body, err = io.ReadAll(req.Body)
			}
			second <- read{body, err}
		}()
		synctest.Wait()
	}
	var secondRead *read
	if second != nil {
		select {
		case r := <-second:
			secondRead = &r
		default:
			findings = append(findings, "the next post's request did not arrive while the first post's reply was withheld")
		}
	}
	post, err := g.named(first, body)
	if err != nil {
		g.errs = append(g.errs, err)
		_ = first.Close()
		return findings
	}
	g.record(post)
	if second == nil {
		return findings
	}
	if secondRead == nil {
		// Virtual time; the bound only aborts a sender that never writes its next request.
		select {
		case r := <-second:
			secondRead = &r
		case <-time.After(time.Hour):
			g.errs = append(g.errs, errors.New("the next post's request never arrived after the first reply"))
			_ = next[0].Close()
			return findings
		}
	}
	var nextPost gatePost
	if err = secondRead.err; err == nil {
		nextPost, err = g.named(next[0], secondRead.body)
	}
	if err != nil {
		g.errs = append(g.errs, fmt.Errorf("read the next post: %w", err))
		_ = next[0].Close()
		return findings
	}
	g.record(nextPost)
	return findings
}

// readHead reads a request line and headers one byte at a time, through their blank line.
func readHead(conn net.Conn) ([]byte, error) {
	var head []byte
	b := make([]byte, 1)
	for !bytes.HasSuffix(head, []byte("\r\n\r\n")) {
		if len(head) >= maxPostHead {
			return nil, fmt.Errorf("request headers exceed %d bytes", maxPostHead)
		}
		if _, err := io.ReadFull(conn, b); err != nil {
			return nil, err
		}
		head = append(head, b[0])
	}
	return head, nil
}

// Serve answers posts until waited is closed and no post is parked, so that no connection is left
// open when the bubble ends on every path where the sender posts or lets its wait return. The
// bound is virtual time and only aborts a sender that does neither.
func (g *OrderGate) Serve(t testing.TB, waited <-chan struct{}) {
	t.Helper()
	abort := time.After(time.Hour)
	for {
		synctest.Wait()
		if g.take() {
			continue
		}
		select {
		case <-waited:
			return
		case <-g.dialed:
		case <-abort:
			t.Error("the exporter neither posted nor finished within an hour of virtual time")
			return
		}
	}
}

// Check reports the collector's failures, then compares the records it took with want: first as
// sets, so that a lost or extra record is told apart from a reordering, then in order.
func (g *OrderGate) Check(t testing.TB, want []string) {
	t.Helper()
	for _, err := range g.errs {
		t.Errorf("order gate collector: %v", err)
	}
	sortedWant, sortedGot := slices.Sorted(slices.Values(want)), slices.Sorted(slices.Values(g.keys))
	if d := gocmp.Diff(sortedWant, sortedGot); d != "" {
		t.Errorf("records the collector received, ignoring order (-want +got):\n%s", d)
		return
	}
	if d := gocmp.Diff(want, g.keys); d != "" {
		t.Errorf("records reached the collector out of submission order (-submitted +received):\n%s", d)
	}
}
