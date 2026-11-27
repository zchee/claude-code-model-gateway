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

//go:build !unix

package lifecycle

import (
	"context"
	"errors"
)

// Acquire returns an unsupported-operation error: this system has no advisory file lock the fence could rely on,
// and the fence never claims an exclusion it cannot give.
func (f *StartFence) Acquire(ctx context.Context) (release func() error, err error) {
	return nil, errors.ErrUnsupported
}

func unlockFenceDescriptor(int) error { return errors.ErrUnsupported }
