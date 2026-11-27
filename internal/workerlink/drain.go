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
	"context"
	"errors"
	"io"
	"net"
	"os"
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

// EncodeDrain writes one complete newline-terminated message without closing the writer.
func EncodeDrain(w io.Writer, msg DrainMessage) error { panic("not implemented") }

// DecodeDrain consumes one bounded newline-terminated message from a persistent buffered reader.
func DecodeDrain(r *bufio.Reader) (DrainMessage, error) { panic("not implemented") }

// DrainChannel lends the reservation's parent connection and transfers its child descriptor to the caller once.
func (r *Reservation) DrainChannel() (parent net.Conn, child *os.File, err error) {
	panic("not implemented")
}

// ImportDrainChannel consumes the inherited file and returns the worker's owned duplex connection.
func ImportDrainChannel(file *os.File) (net.Conn, error) { panic("not implemented") }

// AcknowledgeDrain waits for admitted dials, writes the retirement acknowledgment and releases the parent's listener.
func (r *Reservation) AcknowledgeDrain(ctx context.Context) error { panic("not implemented") }
