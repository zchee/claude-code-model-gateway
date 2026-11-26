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

// Package proxyreq prepares classified proxy requests and answers local refusals.
package proxyreq

import (
	"errors"
	"net/http"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

// OverflowWriter reports a synthetic overflow to the client and the request's route telemetry.
type OverflowWriter interface {
	WriteSentryOverflow(http.ResponseWriter, *http.Request, []byte, Result, sentry.Crossing)
}

// Options supplies worker-lifetime dependencies; relative dispatch paths resolve against StartupDir.
type Options struct {
	StartupDir, DispatchPath string
	Lookup                   config.LookupEnv
	Now                      func() time.Time
	Sentry                   *sentry.State
	Windows                  policy.Windows
	Reply                    OverflowWriter
}

// Result describes the effective route and either the serialized upstream body or a local response.
type Result struct {
	Body                                         []byte
	SelectedModel, EffectiveModel, Provider, Via string
	Effort                                       *string
	// Ticket attributes usage to a dispatch ticket, except on the turn inheriting another agent's route.
	Ticket      *string
	SessionKey  string
	Target      sentry.Target
	LocalStatus int
	Reason      string
}

// Handler owns one worker's dispatch cache and shared request settings.
type Handler struct{}

// New constructs the worker's request handler without consulting ambient paths or configuration.
func New(options Options) (*Handler, error) { return &Handler{}, nil }

// Prepare writes local refusals or returns a forwardable result with zero LocalStatus.
// The caller supplies the original bytes and the same snapshot used to classify them.
func (h *Handler) Prepare(w http.ResponseWriter, r *http.Request, snapshot route.Snapshot, decision route.Decision, original []byte) (Result, error) {
	return Result{}, errors.New("not implemented")
}
