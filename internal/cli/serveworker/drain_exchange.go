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
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// drainExchangeBudget bounds the whole drain exchange, the notice's write and the answer's read together: the
// supervisor's answer is due within the same five seconds a worker has to report its listener.
const drainExchangeBudget = 5 * time.Second

// aLongTimeAgo is a deadline in the past, which ends an operation in progress at once.
var aLongTimeAgo = time.Unix(1, 0)

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
	if err := ctx.Err(); err != nil {
		return err
	}
	// One deadline for both directions, set once: nothing below restarts it.
	if err := conn.SetDeadline(time.Now().Add(drainExchangeBudget)); err != nil {
		return fmt.Errorf("set the drain exchange's deadline: %w", err)
	}
	// A context that ends moves the deadline into the past, which ends the write or read in progress at once. The
	// callback is joined before return, so it never touches conn afterwards.
	interrupted := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(interrupted)
		_ = conn.SetDeadline(aLongTimeAgo)
	})
	defer func() {
		if !stop() {
			<-interrupted
		}
	}()
	err := exchangeDrain(conn, generation)
	if ctxErr := ctx.Err(); ctxErr != nil && err != nil {
		return ctxErr
	}
	return err
}

// exchangeDrain writes the notice once and decides on the first answer frame.
func exchangeDrain(conn net.Conn, generation workerlink.Generation) error {
	if err := workerlink.EncodeDrain(conn, workerlink.DrainMessage{Type: workerlink.Draining, Generation: generation}); err != nil {
		return fmt.Errorf("could not write the drain notice: %w", err)
	}
	msg, err := workerlink.DecodeDrain(bufio.NewReader(conn))
	switch {
	case err != nil:
		return fmt.Errorf("no answer to the drain notice: %w", err)
	case msg.Type != workerlink.Retired:
		return fmt.Errorf("%w: an unexpected %q frame in place of the answer", errUnexpectedDrainAnswer, msg.Type)
	case msg.Generation != generation:
		return fmt.Errorf("%w: a retirement for another generation", errUnexpectedDrainAnswer)
	}
	return nil
}
