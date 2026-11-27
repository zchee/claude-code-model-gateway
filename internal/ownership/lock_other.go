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

package ownership

import (
	"context"
	"errors"
	"fmt"

	"github.com/zchee/claude-code-model-gateway/internal/atomicfile"
)

// lockRecords refuses on a platform without advisory file locks rather than claim an exclusion it cannot give.
func lockRecords(_ context.Context, _ *atomicfile.Dir, root string, _ bool) (release func() error, err error) {
	return nil, fmt.Errorf("%s in %s: %w", recordLockName, root, errors.ErrUnsupported)
}
