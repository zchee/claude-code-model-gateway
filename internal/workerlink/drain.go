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

package workerlink

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"
	"unicode/utf8"
)

// DrainLineLimit bounds one encoded message in bytes, including its terminating LF.
const DrainLineLimit = 256

// Draining identifies the worker's request to withdraw admission.
const Draining = "draining"

// Retired identifies the supervisor's acknowledgment of retired admission.
const Retired = "retired"

// DrainMessage names a transition for exactly one worker generation.
type DrainMessage struct {
	// Type is either Draining or Retired.
	Type string `json:"type"`
	// Generation identifies the reservation that owns the channel.
	Generation Generation `json:"generation"`
}

// ErrDrainMessage denotes an invalid or incomplete drain frame.
var ErrDrainMessage = errors.New("invalid worker drain message")

// ErrDrainLineTooLong denotes a drain frame that cannot fit together with its terminating LF.
var ErrDrainLineTooLong = errors.New("worker drain line too long")

// aLongTimeAgo is a write deadline that has passed, which ends a Write in progress at once.
var aLongTimeAgo = time.Unix(1, 0)

// drainLine validates msg and returns its complete compact frame: the type member, then the generation member, then
// one LF.
func drainLine(msg DrainMessage) ([]byte, error) {
	if msg.Type != Draining && msg.Type != Retired {
		return nil, fmt.Errorf("%w: unknown type %q", ErrDrainMessage, msg.Type)
	}
	if msg.Generation == "" {
		return nil, fmt.Errorf("%w: the generation is empty", ErrDrainMessage)
	}
	// Every byte of the generation takes at least one byte on the wire, so a generation longer than the room the
	// frame leaves is refused before it is scanned or escaped, and its value stays out of the error.
	if room := DrainLineLimit - len(`{"type":"`+msg.Type+`","generation":""}`+"\n"); len(msg.Generation) > room {
		return nil, fmt.Errorf("%w: a generation of %d bytes, at most %d fit", ErrDrainLineTooLong, len(msg.Generation), room)
	}
	if !utf8.ValidString(string(msg.Generation)) {
		return nil, fmt.Errorf("%w: the generation is not UTF-8", ErrDrainMessage)
	}
	line := make([]byte, 0, DrainLineLimit)
	line = append(line, `{"type":`...)
	line, err := jsontext.AppendQuote(line, msg.Type)
	if err != nil {
		return nil, fmt.Errorf("%w: the type cannot be quoted", ErrDrainMessage)
	}
	line = append(line, `,"generation":`...)
	if line, err = jsontext.AppendQuote(line, msg.Generation); err != nil {
		return nil, fmt.Errorf("%w: the generation cannot be quoted", ErrDrainMessage)
	}
	line = append(line, "}\n"...)
	if len(line) > DrainLineLimit {
		return nil, fmt.Errorf("%w: %d bytes, the limit is %d", ErrDrainLineTooLong, len(line), DrainLineLimit)
	}
	return line, nil
}

// EncodeDrain writes one complete newline-terminated message without closing the writer.
//
// The message is validated and bounded before the single Write, so a refused message writes nothing. A Write that
// takes only part of the frame without an error is reported as io.ErrShortWrite, never as success.
func EncodeDrain(w io.Writer, msg DrainMessage) error {
	line, err := drainLine(msg)
	if err != nil {
		return err
	}
	n, err := w.Write(line)
	if err != nil {
		return fmt.Errorf("workerlink: write drain message: %w", err)
	}
	if n != len(line) {
		return fmt.Errorf("workerlink: write drain message: %d of %d bytes: %w", n, len(line), io.ErrShortWrite)
	}
	return nil
}

// DecodeDrain consumes one bounded newline-terminated message from a persistent buffered reader.
//
// It consumes only the bytes of that one line, so bytes already buffered after it stay for the next call, and it
// answers as soon as the line is complete or can no longer fit, never waiting for end of file. Zero bytes then end of
// file give io.EOF; a partial line then end of file, or a line that is not exactly one valid message, gives
// ErrDrainMessage; a line without LF within DrainLineLimit bytes gives ErrDrainLineTooLong; a failure of the reader
// itself is returned wrapped and matches none of these. Every error comes with the zero message.
func DecodeDrain(r *bufio.Reader) (DrainMessage, error) {
	line, err := readDrainLine(r)
	if err != nil {
		return DrainMessage{}, err
	}
	msg, err := parseDrainLine(line[:len(line)-1])
	if err != nil {
		return DrainMessage{}, err
	}
	return msg, nil
}

// readDrainLine returns one line including its LF. It reads only what the reader has buffered or what one fill
// brings, so it consumes no byte past the line's LF and none past the limit.
func readDrainLine(r *bufio.Reader) ([]byte, error) {
	line := make([]byte, 0, DrainLineLimit)
	for len(line) < DrainLineLimit {
		if r.Buffered() == 0 {
			if _, err := r.Peek(1); err != nil {
				switch {
				case errors.Is(err, io.EOF) && len(line) == 0:
					return nil, io.EOF
				case errors.Is(err, io.EOF):
					return nil, fmt.Errorf("%w: end of file after %d bytes without a line feed", ErrDrainMessage, len(line))
				default:
					return nil, fmt.Errorf("workerlink: read drain message: %w", err)
				}
			}
		}
		window, _ := r.Peek(min(r.Buffered(), DrainLineLimit-len(line)))
		if i := bytes.IndexByte(window, '\n'); i >= 0 {
			line = append(line, window[:i+1]...)
			_, _ = r.Discard(i + 1)
			return line, nil
		}
		line = append(line, window...)
		_, _ = r.Discard(len(window))
	}
	return nil, fmt.Errorf("%w: no line feed within %d bytes", ErrDrainLineTooLong, DrainLineLimit)
}

// parseDrainLine parses the body of one line, without its LF, as exactly one message.
func parseDrainLine(body []byte) (DrainMessage, error) {
	// JSON counts a carriage return as white space, so a line ended by CRLF would otherwise pass. Only a CR right
	// before the LF is that terminator; any other CR is white space between tokens or, inside a string, refused by
	// the parser.
	if len(body) > 0 && body[len(body)-1] == '\r' {
		return DrainMessage{}, fmt.Errorf("%w: the line ends with CRLF", ErrDrainMessage)
	}
	var msg DrainMessage
	// The defaults of encoding/json/v2 already refuse invalid UTF-8 and lone surrogate escapes, duplicate member
	// names (compared after unescaping), case-insensitive name matches and trailing data; unknown members are refused
	// here too.
	if err := json.Unmarshal(body, &msg, json.RejectUnknownMembers(true)); err != nil {
		// The parser's text is kept but not its error chain, which may wrap io.ErrUnexpectedEOF and similar errors
		// a caller must not mistake for the state of the channel.
		return DrainMessage{}, fmt.Errorf("%w: %s", ErrDrainMessage, err.Error())
	}
	// A missing member and a JSON null both leave the zero value, which no valid message has.
	if msg.Type != Draining && msg.Type != Retired {
		return DrainMessage{}, fmt.Errorf("%w: unknown or missing type", ErrDrainMessage)
	}
	if msg.Generation == "" || !utf8.ValidString(string(msg.Generation)) {
		return DrainMessage{}, fmt.Errorf("%w: empty or missing generation", ErrDrainMessage)
	}
	return msg, nil
}

// DrainChannel lends the reservation's parent connection and transfers its child descriptor to the caller once.
//
// The parent connection stays the reservation's: the manager is its only reader, AcknowledgeDrain makes its only
// write and End closes it. The child file becomes the caller's, who closes it once the worker has started; after the
// transfer the reservation keeps no copy of the child's end.
func (r *Reservation) DrainChannel() (parent net.Conn, child *os.File, err error) {
	r.reg.mu.Lock()
	defer r.reg.mu.Unlock()
	switch {
	case r.ended:
		return nil, nil, fmt.Errorf("workerlink: generation %q has ended: %w", r.generation, ErrUnavailable)
	case r.channelTaken:
		return nil, nil, fmt.Errorf("workerlink: the drain channel of generation %q was already taken", r.generation)
	}
	r.channelTaken = true
	child = r.child
	r.child = nil
	return r.parent, child, nil
}

// ImportDrainChannel consumes the inherited file and returns the worker's owned duplex connection.
//
// The file is closed on success and on failure; it is marked close-on-exec before anything else is checked, and the
// returned connection holds its own close-on-exec duplicate. Only the kind of socket is checked, a connected local
// stream socket, not whether both directions are still open.
func ImportDrainChannel(file *os.File) (net.Conn, error) {
	if file == nil {
		return nil, errors.New("workerlink: no inherited drain channel descriptor")
	}
	c, err := importDrainChannel(file)
	if err != nil {
		return nil, fmt.Errorf("workerlink: inherited drain channel refused: %w", err)
	}
	return c, nil
}

// AcknowledgeDrain waits for admitted dials, writes the retirement acknowledgment and releases the parent's listener.
//
// It refuses before writing unless the generation was retired and has not ended. It waits, outside the registry's
// lock, until every dial that pinned the generation has settled, giving up when ctx ends or End begins; then one
// caller claims the write and every concurrent caller joins that writer through its channel, so one line at most is
// ever written. A cancellation, a passed caller deadline or an End that comes before the Write is admitted makes no
// Write call and releases the claim, so a later call may try again. Once a Write was entered its outcome is final: a
// failed acknowledgment keeps the listener and is never retried, because part of the line may be on the channel.
// The write is bounded by the earlier of the caller's deadline and the registry's budget, and ends at once when ctx
// ends or End closes the channel; the first of these interruptions decides the error, and the call returns only
// after its Write has returned. Success is reported only after the listener's Close has returned. Issued leases and
// their Exited channels are left alone.
func (r *Reservation) AcknowledgeDrain(ctx context.Context) error {
	reg := r.reg
	for {
		reg.mu.Lock()
		if err := ctx.Err(); err != nil {
			reg.mu.Unlock()
			return err
		}
		switch {
		case r.ended:
			reg.mu.Unlock()
			return r.endedAckError()
		case !r.retired:
			reg.mu.Unlock()
			return fmt.Errorf("workerlink: no acknowledgment for generation %q, whose admission was not retired", r.generation)
		case r.ack == ackSent:
			reg.mu.Unlock()
			return nil
		case r.ack == ackFailed:
			err := r.ackErr
			reg.mu.Unlock()
			return err
		case r.ack == ackWriting:
			done := r.ackDone
			reg.mu.Unlock()
			select {
			case <-done:
			case <-ctx.Done():
				return ctx.Err()
			}
			continue
		case r.pins > 0:
			reg.mu.Unlock()
			if err := r.awaitPins(ctx, r.endCtx.Done()); err != nil {
				return err
			}
			continue
		}
		r.ack = ackWriting
		r.ackDone = make(chan struct{})
		r.ackCause, r.ackCauseAt, r.ackInterruptErr = nil, time.Time{}, nil
		reg.mu.Unlock()
		return r.writeAck(ctx)
	}
}

// errAckBudget is the cause of an acknowledgment whose write outlived the registry's budget.
var errAckBudget = errors.New("the drain acknowledgment's write budget is spent")

// endedAckError is the error of an acknowledgment that the generation's End refused or interrupted.
func (r *Reservation) endedAckError() error {
	return fmt.Errorf("workerlink: no acknowledgment for generation %q, which has ended: %w", r.generation, ErrUnavailable)
}

// interruptAckLocked records cause as the interruption of the claimed acknowledgment write, unless an earlier one
// was recorded: the first interruption decides the write's error.
func (r *Reservation) interruptAckLocked(cause error) {
	if r.ack == ackWriting && r.ackCause == nil {
		r.ackCause, r.ackCauseAt = cause, time.Now()
	}
}

// writeAck writes the one acknowledgment line as the claimed writer. It arms the write deadline, admits the Write
// only if nothing has interrupted it yet, classifies the outcome by the first interruption, releases the listener
// when the line went out completely, and then records the outcome and wakes the joiners.
func (r *Reservation) writeAck(ctx context.Context) error {
	reg := r.reg
	parent := r.parent
	line, err := drainLine(DrainMessage{Type: Retired, Generation: r.generation})
	if err != nil {
		// Reserve refuses a generation that cannot be framed, so this is not reached; nothing was written.
		return r.releaseAckClaim(fmt.Errorf("workerlink: acknowledgment of generation %q: %w", r.generation, err))
	}
	deadline, callerDeadline := time.Now().Add(reg.ackBudget), false
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline, callerDeadline = d, true
	}
	// Only the write deadline: the manager may be reading this connection at the same time.
	if err := parent.SetWriteDeadline(deadline); err != nil {
		return r.releaseAckClaim(fmt.Errorf("workerlink: acknowledgment of generation %q: its write deadline cannot be armed: %w", r.generation, err))
	}
	interrupted := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(interrupted)
		reg.mu.Lock()
		r.interruptAckLocked(fmt.Errorf("workerlink: acknowledgment of generation %q interrupted: %w", r.generation, ctx.Err()))
		reg.mu.Unlock()
		if err := parent.SetWriteDeadline(aLongTimeAgo); err != nil {
			// The Write cannot be ended through its deadline, so it is ended by closing the channel.
			closeErr := parent.Close()
			reg.mu.Lock()
			r.ackInterruptErr = errors.Join(fmt.Errorf("workerlink: end the acknowledgment write through its deadline: %w", err), closeErr)
			reg.mu.Unlock()
		}
	})
	joinCallback := func() {
		if !stop() {
			<-interrupted
		}
	}

	// The admission point: the last check before the Write, made under the lock that the cancellation callback and
	// End take to record an interruption, so that one recorded before here makes no Write call.
	reg.mu.Lock()
	refusal := r.ackCause
	switch {
	case refusal != nil:
	case r.ended:
		refusal = r.endedAckError()
	case ctx.Err() != nil:
		refusal = fmt.Errorf("workerlink: acknowledgment of generation %q interrupted: %w", r.generation, ctx.Err())
	case callerDeadline && !time.Now().Before(deadline):
		refusal = fmt.Errorf("workerlink: acknowledgment of generation %q interrupted: %w", r.generation, context.DeadlineExceeded)
	}
	reg.mu.Unlock()
	if refusal != nil {
		joinCallback()
		return r.releaseAckClaim(refusal)
	}

	n, werr := parent.Write(line)
	returned := time.Now()
	// The callback is joined before the interruption is read: when it ends the Write by closing the channel, it
	// records why only after that close.
	joinCallback()
	reg.mu.Lock()
	cause, causeAt, interruptErr := r.ackCause, r.ackCauseAt, r.ackInterruptErr
	reg.mu.Unlock()

	switch {
	case werr == nil && n == len(line):
		err = nil
	case werr == nil:
		err = fmt.Errorf("workerlink: acknowledgment of generation %q: %d of %d bytes written: %w", r.generation, n, len(line), io.ErrShortWrite)
	default:
		// An interruption recorded after the Write returned did not end it.
		recorded := cause != nil && !causeAt.After(returned)
		timedOut := errors.Is(werr, os.ErrDeadlineExceeded)
		switch {
		case timedOut && (!recorded || !causeAt.Before(deadline)):
			// The armed deadline passed before any other interruption: it decides.
			if callerDeadline {
				err = fmt.Errorf("workerlink: acknowledgment of generation %q interrupted: %w", r.generation, context.DeadlineExceeded)
			} else {
				err = fmt.Errorf("workerlink: acknowledgment of generation %q not written: %w: %w", r.generation, errAckBudget, werr)
			}
		case recorded && interruptErr != nil:
			err = errors.Join(cause, interruptErr)
		case recorded:
			err = cause
		default:
			err = fmt.Errorf("workerlink: acknowledgment of generation %q not written: %w", r.generation, werr)
		}
	}
	if err == nil {
		reg.mu.Lock()
		release := !r.listenerReleased
		r.listenerReleased = true
		reg.mu.Unlock()
		if release {
			if closeErr := r.listener.Close(); closeErr != nil {
				err = fmt.Errorf("workerlink: acknowledgment of generation %q written, but its listener not released: %w", r.generation, closeErr)
			}
		}
	}
	reg.mu.Lock()
	if err == nil {
		r.ack = ackSent
	} else {
		r.ack, r.ackErr = ackFailed, err
	}
	close(r.ackDone)
	reg.mu.Unlock()
	return err
}

// releaseAckClaim gives up a claim whose Write was never entered: the acknowledgment is left as never tried, the
// joiners wake to try again, and err is returned.
func (r *Reservation) releaseAckClaim(err error) error {
	r.reg.mu.Lock()
	r.ack = ackNone
	r.ackCause, r.ackCauseAt, r.ackInterruptErr = nil, time.Time{}, nil
	close(r.ackDone)
	r.reg.mu.Unlock()
	return err
}
