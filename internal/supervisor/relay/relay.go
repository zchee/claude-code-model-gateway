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

// Package relay forwards a buffered public request to the current worker generation and relays its answer.
package relay

import (
	"context"
	"net/http"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// BodyLimit is the largest request body, inclusive, the relay buffers for the worker (lib/commands.js:2302).
const BodyLimit = 64 << 20

// OversizeGrace bounds how long the relay keeps reading an oversized upload after its 413 head was flushed
// (lib/commands.js:2289).
const OversizeGrace = 250 * time.Millisecond

// Lost describes a request a worker generation took and never answered.
type Lost struct {
	// Generation is the worker generation that took the request.
	Generation workerlink.Generation
	// WorkerPID is the process identifier published for that generation, or zero when it is not known.
	WorkerPID int
	// ErrorType is the sanitized type of the failure, never its message.
	ErrorType string
}

// Options supplies the relay's worker source, its lifecycle report and the supervisor's identity.
type Options struct {
	// Workers gives connection leases to the current worker generation.
	Workers workerlink.Source
	// Lost receives one report for each request a worker took and never answered.
	Lost func(Lost)
	// Wait pauses between two delivery attempts for the given time. It returns the context's error when the request's
	// context ends first. Any other error is the supervisor's stop signal (the source's isStopped,
	// lib/commands.js:2359): the relay makes no further attempt and answers as when its attempts have run out. A stop
	// that first arrives during the wait after a failed attempt counts as a stop already in effect at that failure, so
	// the answer can differ from the source's timing by at most one wait interval. Wait is required: New refuses
	// options without it.
	Wait func(context.Context, time.Duration) error
	// Version is the supervisor version added to the worker's health document.
	Version string
	// SupervisorPID is the supervisor process identifier added to the worker's health document.
	SupervisorPID int
}

// New returns the handler that relays requests to the worker generations of o.Workers.
func New(o Options) (http.Handler, error) { panic("not implemented") }
