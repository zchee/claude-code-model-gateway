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

//go:build unix

package workerlink

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// peerEvidence is what the system reported, step by step, about a local stream socket whose getpeername failed with
// EINVAL. Each error field holds the failure of its step, nil when the step answered. The two steps whose untouched
// fields would read as an accepting answer, the credentials query and the peek, also record whether they were made;
// the zero value therefore refuses the socket at the credentials step, as not observed.
type peerEvidence struct {
	// getpeerErr is the EINVAL getpeername failed with; every refusal keeps it, when set, in its chain.
	getpeerErr error
	// credAsked reports whether the peer-credentials query was made.
	credAsked bool
	// credErr is the result of the LOCAL_PEERCRED query, which answers once the socket has been connected.
	credErr error
	// peerPIDErr is the result of the LOCAL_PEERPID query, which fails with ENOTCONN once no peer is linked.
	peerPIDErr error
	// readEOF reports whether the read filter reported the end of input.
	readEOF bool
	// readReport describes what the read filter reported when it did not report the end of input.
	readReport string
	// readErr is the failure of the read filter query.
	readErr error
	// peekAsked reports whether the receive peek was made.
	peekAsked bool
	// peekN is the byte count of the nonblocking, nonconsuming receive peek: queued bytes, or zero at the end.
	peekN int
	// peekErr is the failure of that peek.
	peekErr error
}

// decide returns nil for a connected local stream socket whose peer has ended and that is still readable to its end:
// one that was connected, is linked to no peer, does not listen, can receive no more, and still answers a receive
// with its queued bytes or the end of input. Otherwise it returns an error naming the step that refused the socket;
// for a socket that has never been connected that error also matches the import's usual "not connected" refusal. A
// step that was not asked refuses the socket at its own place in the order, as not observed.
func (e peerEvidence) decide() error {
	getpeer := os.NewSyscallError("getpeername", e.getpeerErr)
	refuse := func(step string, cause error) error {
		if getpeer == nil {
			return fmt.Errorf("%s: %w", step, cause)
		}
		return fmt.Errorf("%s: %w (%w)", step, cause, getpeer)
	}
	switch {
	case !e.credAsked:
		return refuse("getsockopt LOCAL_PEERCRED", errors.New("not observed: the query was not made"))
	case errors.Is(e.credErr, unix.ENOTCONN):
		return refuse("getsockopt LOCAL_PEERCRED", errNotConnected)
	case e.credErr != nil:
		return refuse("getsockopt LOCAL_PEERCRED", e.credErr)
	case e.peerPIDErr == nil:
		return refuse("getsockopt LOCAL_PEERPID", errors.New("still linked to its peer, a socket that can neither receive nor send"))
	case !errors.Is(e.peerPIDErr, unix.ENOTCONN):
		return refuse("getsockopt LOCAL_PEERPID", e.peerPIDErr)
	case e.readErr != nil:
		return refuse("read filter", e.readErr)
	case !e.readEOF:
		return refuse("read filter", fmt.Errorf("no end of input reported (%s), as for a listening socket", e.readReport))
	case !e.peekAsked:
		return refuse("receive peek", errors.New("not observed: the peek was not made"))
	case e.peekErr != nil:
		return refuse("receive peek", e.peekErr)
	case e.peekN < 0:
		return refuse("receive peek", fmt.Errorf("a byte count of %d", e.peekN))
	}
	return nil
}
