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

// Package worker serves the gateway's local endpoints and routes inference requests.
package worker

import (
	"errors"
	"net"
	"net/http"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
)

// Forward handles a classified request using the same catalog generation and the original body bytes.
type Forward func(http.ResponseWriter, *http.Request, route.Snapshot, route.Decision, []byte)

// Options supplies the worker's startup settings and its three upstream handlers.
type Options struct {
	Version, ControlToken  string
	Lookup                 config.LookupEnv
	Routes                 *route.Store
	Anthropic, Proxy, Grok Forward
	BeginDrain             func()
}

// NewHandler constructs the handler shared by the TCP and Unix listeners.
func NewHandler(options Options) (http.Handler, error) { return nil, errors.New("not implemented") }

// ListenUnix binds path, reclaiming a refused socket but never a live owner or non-socket file.
func ListenUnix(path string) (net.Listener, error) { return nil, errors.New("not implemented") }
