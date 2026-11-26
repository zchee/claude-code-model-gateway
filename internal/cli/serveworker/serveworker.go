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

// Package serveworker owns the serve-worker command's listener and descriptor lifecycle.
package serveworker

import (
	"context"
	"errors"
	"io"

	"github.com/zchee/claude-code-model-gateway/internal/config"
)

// Run serves until drain, parent disconnect, or cancellation, returning a startup or serving error.
func Run(ctx context.Context, lookup config.LookupEnv, stdout, stderr io.Writer) error {
	return errors.New("not implemented")
}
