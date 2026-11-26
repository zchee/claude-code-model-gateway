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

package supervisor

import (
	"bytes"
	"context"
	"net"
	"strconv"
	"strings"
	"sync"
)

// net/http replaces Request.Host with the authority of an absolute-form request target and deletes the Host header
// before any handler runs. The source judges the Host header as the client sent it (lib/commands.js:2801-2813,
// lib/control-auth.js:41-43), so Serve reads every request head on the raw connection a second time, in parallel with
// the server's own parser, and queues each head's Host value for the handler of that request.
//
// The heads are framed exactly as HTTP/1.1 frames them: a body is skipped by its Content-Length or its chunked
// encoding, so a keep-alive connection yields one queue entry per request in order. net/http dispatches the requests of
// one connection sequentially and closes the connection after any request it cannot parse, so the handler of the n-th
// dispatched request receives the n-th queued head.

// maxWireHead bounds one buffered request head. It is above the server's default header limit plus its read buffer,
// so a head the server accepts always fits.
const maxWireHead = 1<<20 + 8<<10

type wireHead struct {
	host  string
	valid bool
}

type headParser struct {
	mu    sync.Mutex
	queue []wireHead
	// broken is set when the stream can no longer be framed; every later request on the connection is refused.
	broken bool

	state     parseState
	line      []byte
	hosts     []string
	lengths   []string
	encodings []string
	sawLine   bool
	headBytes int
	remaining int64
}

type parseState uint8

const (
	stateHead parseState = iota
	stateBody
	stateChunkSize
	stateChunkData
	stateChunkDataEnd
	stateTrailer
)

func (p *headParser) pop() wireHead {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.queue) == 0 {
		return wireHead{}
	}
	head := p.queue[0]
	p.queue = p.queue[1:]
	return head
}

func (p *headParser) feed(data []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(data) > 0 && !p.broken {
		switch p.state {
		case stateBody, stateChunkData:
			n := min(int64(len(data)), p.remaining)
			p.remaining -= n
			data = data[n:]
			if p.remaining == 0 {
				if p.state == stateBody {
					p.state = stateHead
				} else {
					p.state = stateChunkDataEnd
				}
			}
		default:
			i := bytes.IndexByte(data, '\n')
			if i < 0 {
				p.appendLine(data)
				data = nil
				continue
			}
			p.appendLine(data[:i])
			data = data[i+1:]
			if p.broken {
				return
			}
			line := bytes.TrimSuffix(p.line, []byte("\r"))
			p.line = p.line[:0]
			p.endLine(line)
		}
	}
}

func (p *headParser) appendLine(b []byte) {
	p.headBytes += len(b) + 1
	if p.headBytes > maxWireHead {
		p.fail()
		return
	}
	p.line = append(p.line, b...)
}

func (p *headParser) fail() {
	p.broken = true
	p.line = nil
}

func (p *headParser) endLine(line []byte) {
	switch p.state {
	case stateHead:
		p.headLine(line)
	case stateChunkSize:
		size, _, _ := bytes.Cut(line, []byte(";"))
		n, err := strconv.ParseInt(string(bytes.TrimSpace(size)), 16, 64)
		if err != nil || n < 0 {
			p.fail()
			return
		}
		p.headBytes = 0
		if n == 0 {
			p.state = stateTrailer
			return
		}
		p.remaining, p.state = n, stateChunkData
	case stateChunkDataEnd:
		if len(line) != 0 {
			p.fail()
			return
		}
		p.headBytes, p.state = 0, stateChunkSize
	case stateTrailer:
		if len(line) == 0 {
			p.headBytes, p.state = 0, stateHead
		}
	}
}

func (p *headParser) headLine(line []byte) {
	if !p.sawLine {
		if len(line) == 0 {
			// Empty lines before a request line are tolerated, not counted.
			p.headBytes = 0
			return
		}
		p.sawLine = true
		return
	}
	if len(line) > 0 {
		name, value, ok := bytes.Cut(line, []byte(":"))
		if !ok {
			return
		}
		v := strings.Trim(string(value), " \t")
		switch strings.ToLower(string(name)) {
		case "host":
			p.hosts = append(p.hosts, v)
		case "content-length":
			p.lengths = append(p.lengths, v)
		case "transfer-encoding":
			p.encodings = append(p.encodings, v)
		}
		return
	}
	head := wireHead{valid: len(p.hosts) == 1}
	if head.valid {
		head.host = p.hosts[0]
	}
	p.queue = append(p.queue, head)
	chunked := len(p.encodings) > 0
	var length int64
	if !chunked && len(p.lengths) > 0 {
		n, err := strconv.ParseInt(p.lengths[0], 10, 64)
		if err != nil || n < 0 {
			p.fail()
			return
		}
		length = n
	}
	p.hosts, p.lengths, p.encodings, p.sawLine, p.headBytes = nil, nil, nil, false, 0
	switch {
	case chunked:
		p.state = stateChunkSize
	case length > 0:
		p.remaining, p.state = length, stateBody
	}
}

type headConn struct {
	net.Conn
	parser *headParser
}

func (c *headConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.parser.feed(b[:n])
	}
	return n, err
}

type headListener struct{ net.Listener }

func (l headListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &headConn{Conn: conn, parser: new(headParser)}, nil
}

type headKey struct{}

func withHeadParser(ctx context.Context, conn net.Conn) context.Context {
	if c, ok := conn.(*headConn); ok {
		return context.WithValue(ctx, headKey{}, c.parser)
	}
	return ctx
}

// wireHost returns the Host header of this request as the client sent it. A request that did not arrive through Serve
// keeps the Host value the caller placed in the request.
func wireHost(ctx context.Context, parsed string) (string, bool) {
	p, ok := ctx.Value(headKey{}).(*headParser)
	if !ok {
		return parsed, true
	}
	head := p.pop()
	return head.host, head.valid
}
