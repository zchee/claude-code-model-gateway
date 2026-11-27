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

package procinfo

import (
	"context"
	"math"
	"os"
	"runtime"
)

// StartToken is evidence about one lifetime of a process number, independent
// of the wall clock. It is opaque and compared for equality only; an empty
// token is unreadable evidence and never matches. A process number together
// with a token is not a unique handle and not an authority to signal: two
// processes can share a start tick on Linux or a microsecond on macOS. A token
// does not reveal an exec.
type StartToken string

// StartToken returns the process-start token of pid. It reads only what the
// token is made of: on Linux the boot identifier and the start field of stat,
// on macOS the start the kernel recorded for the process. Every error comes
// with an empty token: os.ErrNotExist for confirmed absence, ErrUnsupported
// without a native probe, and otherwise an error matching ErrUnknown. It uses
// Identity's budget.
func (p Probe) StartToken(ctx context.Context, pid int) (StartToken, error) {
	if pid <= 0 || pid > math.MaxInt32 {
		return "", os.ErrNotExist
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout(runtime.GOOS))
	defer cancel()
	if ctx.Err() != nil {
		return "", p.inspectionError(ctx, pid, nil)
	}
	token, err := nativeStartToken(ctx, pid)
	if err == nil && ctx.Err() == nil && token != "" {
		return token, nil
	}
	return "", p.inspectionError(ctx, pid, err)
}
