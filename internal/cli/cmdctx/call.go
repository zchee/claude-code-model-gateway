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

// Package cmdctx defines the inputs passed to gateway commands.
package cmdctx

import (
	"context"
	"io"

	"github.com/zchee/claude-code-model-gateway/internal/cli/argscan"
	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/paths"
)

// Call supplies lazy process facts, streams and the gate's interruptible read.
type Call struct {
	Paths      paths.Resolver
	Lookup     config.LookupEnv
	Environ    func() []string
	Executable func() (string, error)
	Version    func() string
	Cwd        func() (string, error)
	Stdin      io.Reader
	ReadInput  func(context.Context) ([]byte, error)
	Streams    argscan.Streams
}
