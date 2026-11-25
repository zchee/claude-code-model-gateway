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
	"context"
	"os/exec"
	"slices"
	"sync"
	"time"
)

const (
	fixtureTimeout   = 5 * time.Second
	fixtureWaitDelay = time.Second
)

// UsageRequest records the parts of a request observed by a fixture collector.
type UsageRequest struct {
	Method, RequestURI, ContentType, Body string
}

// UsageCollector records requests sent to its private loopback URL without forwarding them.
// A copy refers to the same live collector and observes the same requests.
type UsageCollector struct {
	URL   string
	state *usageCollectorState
}

// RunFixture authorizes a fixture to send usage to this collector while it is running.
// The caller must set the collector URL in env. The fixture bounds the child lifetime without extending any caller deadline.
func (c *UsageCollector) RunFixture(env Environment, cmd *exec.Cmd) error {
	return env.runFixture(cmd, c.state)
}

// Requests returns a snapshot of the requests recorded by the collector.
func (c *UsageCollector) Requests() []UsageRequest {
	if c.state == nil {
		return nil
	}
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	return slices.Clone(c.state.requests)
}

type usageCollectorState struct {
	endpoint string
	stopped  <-chan struct{}
	ctx      context.Context

	mu       sync.Mutex
	active   int
	idle     chan struct{}
	requests []UsageRequest
}

func (s *usageCollectorState) run(cmd *exec.Cmd) error {
	s.mu.Lock()
	if s.ctx.Err() != nil {
		s.mu.Unlock()
		return ErrUsageEndpoint
	}
	select {
	case <-s.stopped:
		s.mu.Unlock()
		return ErrUsageEndpoint
	default:
	}
	if s.active == 0 {
		s.idle = make(chan struct{})
	}
	s.active++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.active--
		if s.active == 0 {
			close(s.idle)
		}
		s.mu.Unlock()
	}()

	// Cmd retains its original, private context. Enforce the fixture deadline separately.
	ctx, cancel := context.WithTimeout(s.ctx, fixtureTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	if cmd.WaitDelay <= 0 || cmd.WaitDelay > fixtureWaitDelay {
		cmd.WaitDelay = fixtureWaitDelay
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	process := cmd.Process
	stop := context.AfterFunc(ctx, func() { _ = process.Kill() })
	defer stop()
	return cmd.Wait()
}
