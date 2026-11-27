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

package serveworker

import (
	"context"
	"errors"
	"net"

	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// errUnexpectedDrainAnswer is the cause of an exchange whose answer is a well-formed drain frame other than the
// retirement of the worker's own generation: a frame in the wrong direction or for another generation.
var errUnexpectedDrainAnswer = errors.New("the drain answer is not the retirement of this generation")

// drainExchange performs the worker's side of the drain exchange on conn: it writes the draining notice for generation
// once and decides on the first answer frame alone, all within one budget of five seconds counted from its start;
// writing the notice and reading the answer share that budget, and the exchange does not wait for a second frame. It
// returns nil only for the retirement of generation. A budget that runs out fails with a deadline error
// (os.ErrDeadlineExceeded or context.DeadlineExceeded), a malformed or incomplete frame with
// workerlink.ErrDrainMessage, an end of file before any byte of the answer with io.EOF, any other well-formed frame at
// once with errUnexpectedDrainAnswer, and a context that ends first with the context's error.
func drainExchange(ctx context.Context, conn net.Conn, generation workerlink.Generation) error {
	panic("not implemented")
}
