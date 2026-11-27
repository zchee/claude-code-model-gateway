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

import "context"

// StartToken identifies one lifetime of a process number independently of the
// wall clock. It is opaque and compared for equality only; an empty token is
// unreadable evidence and never matches. A token does not reveal an exec.
type StartToken string

// StartToken returns the process-start token of pid, or an empty token and an
// error when the token cannot be read.
func (p Probe) StartToken(ctx context.Context, pid int) (StartToken, error) {
	panic("not implemented")
}
