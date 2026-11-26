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

// Package supervisor serves the public listener in front of the request worker.
package supervisor

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
)

// NewHandler constructs a handler for an already bound public port. workerPort returns the current worker's port.
func NewHandler(publicPort int, workerPort func() int, token, version string, pid int) (http.Handler, error) {
	return nil, errors.New("not implemented")
}

// Serve dispatches requests received on the bound public listener until cancellation or listener failure.
func Serve(ctx context.Context, listener net.Listener, handler http.Handler) error {
	return errors.New("not implemented")
}

// FixtureShutdown consumes and closes the inherited fixture channel. Without a test guard it closes it unread.
func FixtureShutdown(channel io.ReadCloser, guarded bool) (bool, error) {
	return false, errors.New("not implemented")
}
