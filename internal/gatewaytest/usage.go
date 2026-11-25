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
	"errors"
	"os/exec"
	"slices"
	"sync"
	"time"
)

const (
	fixtureTimeout    = 5 * time.Second
	maxFixtureTimeout = 30 * time.Second
	fixtureWaitDelay  = time.Second
)

// ErrInvalidFixtureLimit identifies a fixture launch limit outside (0, 30s].
var ErrInvalidFixtureLimit = errors.New("fixture limit must be positive and at most 30 seconds")

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

// RunFixture authorizes a fixture to send usage to this collector with a five-second limit.
func (c *UsageCollector) RunFixture(env Environment, cmd *exec.Cmd) error {
	return c.RunFixtureWithin(env, cmd, fixtureTimeout)
}

// RunFixtureWithin authorizes a fixture with a positive limit of at most 30 seconds.
// The caller must set the collector URL in env. Earlier caller deadlines and collector cancellation still apply.
func (c *UsageCollector) RunFixtureWithin(env Environment, cmd *exec.Cmd, limit time.Duration) error {
	return env.runFixture(cmd, c.state, limit)
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

func (s *usageCollectorState) run(cmd *exec.Cmd, limit time.Duration) error {
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
	return runBoundedFixture(s.ctx, cmd, limit)
}

func runBoundedFixture(parent context.Context, cmd *exec.Cmd, limit time.Duration) error {
	// Cmd retains its original, private context. Enforce the fixture deadline separately.
	ctx, cancel := context.WithTimeout(parent, limit)
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
	err := cmd.Wait()
	if !stop() {
		return errors.Join(ctx.Err(), err)
	}
	return err
}
