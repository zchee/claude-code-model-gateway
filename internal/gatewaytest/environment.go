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

// Package gatewaytest isolates gateway fixtures and guards product operations under test.
package gatewaytest

import (
	"context"
	"errors"
	"net"
	"os/exec"
)

var (
	// ErrReservedPort identifies a bind refused before opening a socket.
	ErrReservedPort = errors.New("reserved gateway port")
	// ErrRealClaude identifies a launch refused before starting Claude.
	ErrRealClaude = errors.New("refusing to launch real claude")
	// ErrOutsideSandbox identifies settings access outside the test sandbox.
	ErrOutsideSandbox = errors.New("refusing to touch settings outside the sandbox")
	errNotImplemented = errors.New("gateway test isolation not implemented")
)

// Environment is the environment of a gateway process, not its parent's live environment.
type Environment map[string]string

// NewEnv builds an isolated environment in the caller's temporary home. Isolated overrides apply last.
func NewEnv(home string, parent, overrides, isolatedOverrides map[string]string) Environment {
	return nil
}

// RunFixture runs a context-bound command with this exact child environment.
func (env Environment) RunFixture(cmd *exec.Cmd) error { return errNotImplemented }

// Listen invokes listen only if the bind is permitted and returns its listener.
func (env Environment) Listen(ctx context.Context, network, address string, listen func(context.Context, string, string) (net.Listener, error)) (net.Listener, error) {
	return nil, errNotImplemented
}

// StartClaude invokes start with the selected executable only if its path is permitted.
func (env Environment) StartClaude(ctx context.Context, start func(context.Context, string) error) error {
	return errNotImplemented
}

// UserSettingsPath resolves the user settings file subject to the product guard.
func (env Environment) UserSettingsPath() (string, error) { return "", errNotImplemented }

// WriteSettings invokes write with the supplied settings only if the destination is permitted.
func (env Environment) WriteSettings(path string, settings map[string]any, write func(string, map[string]any) error) error {
	return errNotImplemented
}

// ProbeAlias reports the detected model, or nil without spawning when the executable is refused.
func (env Environment) ProbeAlias(ctx context.Context, alias, endpoint string) (*string, error) {
	return nil, errNotImplemented
}

// DetectedPins refreshes the model pins, returning an empty mapping when no model was detected.
func (env Environment) DetectedPins(ctx context.Context) (map[string]string, error) {
	return nil, errNotImplemented
}
