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

// Package serveshim runs the foreground supervisor command.
package serveshim

import (
	"context"
	"errors"
	"io"
)

// Run serves until cancellation, an orderly stop, or a startup failure. The child runs executable with the supplied environment.
func Run(ctx context.Context, environment []string, executable, version string, stdout, stderr io.Writer) error {
	return errors.New("not implemented")
}
