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

//go:build process && (darwin || linux)

package serveshim_test

import (
	"bufio"
	"bytes"
	"net"
	"sync"
	"testing"
	"time"
)

// reportDeadline is how long the supervisor waits for its worker's listening report before it stops.
const reportDeadline = 5 * time.Second

// fixtureChild is one worker process the supervisor started, as seen through its control connection.
type fixtureChild struct {
	pid   int
	conn  net.Conn
	lines *bufio.Scanner
}

// farewell returns the request counts the child sent when its supervisor went away.
func (c *fixtureChild) farewell(t *testing.T) string {
	t.Helper()
	if !c.lines.Scan() {
		t.Fatalf("child closed its control connection without its request counts: %v", c.lines.Err())
	}
	return c.lines.Text()
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
