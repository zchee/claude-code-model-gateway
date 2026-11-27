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

package serveshim

import "context"

// Hooks are the points at which a test holds or observes a supervisor that Run starts with the context WithHooks
// returns. A nil function does nothing.
type Hooks struct {
	// Launching runs at the start of every attempt to launch a worker, before anything is reserved or opened for it.
	Launching func()
	// Starting runs immediately before a worker's process is started, inside whatever serializes worker starts.
	Starting func()
	// Published runs once a worker's eligible pid record is written and before the worker becomes eligible.
	Published func(pid int)
	// ExitObserved runs once the system reported a worker's exit and before the supervisor acts on it; the
	// supervisor acts only once it returns.
	ExitObserved func(pid int)
	// StopInhibited runs once a stop has recorded that no further worker may start, before the stop disconnects or
	// waits for anything.
	StopInhibited func()
	// RelayLookup runs each time the relay actually asks the supervisor for a worker to deliver a request to, with
	// whether one was eligible. It reports lookups that happened, never required ones. It must not block.
	RelayLookup func(eligible bool)
	// LaunchEnded runs once each decision whether to start a worker, the first one or a replacement, has been carried
	// out, with whether a worker process was started.
	LaunchEnded func(started bool)
	// Acknowledging runs just before the supervisor writes its answer to a worker's drain notice; the answer is
	// written only once it returns.
	Acknowledging func()
}

// WithHooks returns a context for Run under which the supervisor calls hooks.
func WithHooks(ctx context.Context, hooks Hooks) context.Context {
	return context.WithValue(ctx, hooksKey{}, testHooks{
		launching: hooks.Launching, starting: hooks.Starting, published: hooks.Published, exitObserved: hooks.ExitObserved,
		stopInhibited: hooks.StopInhibited, relayLookup: hooks.RelayLookup, launchEnded: hooks.LaunchEnded,
		acknowledging: hooks.Acknowledging,
	})
}
