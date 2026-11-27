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
	"context"
	"net/http"
)

// Options supplies the bound public listener's relay, lifecycle actions and supervisor identity.
type Options struct {
	// PublicPort is the actual bound loopback TCP port.
	PublicPort int
	// Token is the validated control token of the supervisor's state root.
	Token string
	// Relay handles traffic not dispatched as a supervisor control operation.
	Relay http.Handler
	// Restart validates a candidate and requests a worker restart.
	Restart func(context.Context, []byte) error
	// Stop synchronously inhibits worker starts and requests ordered shutdown without waiting for exit.
	Stop func()
	// StartupGeneration is the generation published in the supervisor's own instance record.
	StartupGeneration string
	// SignIdentity returns the supervisor proof for the given token, nonce and startup generation.
	SignIdentity func(token, nonce, generation string) (string, error)
}

// NewGateway constructs the dispatcher for an already bound supervisor listener.
func NewGateway(o Options) (http.Handler, error) { panic("not implemented") }
