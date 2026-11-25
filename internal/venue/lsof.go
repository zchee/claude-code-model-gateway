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

package venue

import (
	"context"
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
)

// darwinListeners opens a private loopback listener on a kernel-selected port;
// it never requests a gateway port, accepts a connection, or dials a socket.
// Without this positive control an empty lsof result could mean that lsof cannot
// see this user's listeners. It cannot establish absence for other users.
func darwinListeners() (map[string]bool, error) {
	var lc net.ListenConfig
	listener, err := lc.Listen(context.Background(), "tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	busy, err := inspectLsof(strconv.Itoa(os.Getpid()), port, query)
	if closeErr := listener.Close(); closeErr != nil {
		return nil, closeErr
	}
	return busy, err
}

func inspectLsof(ownPID, canary string, run func(string, ...string) (string, error)) (map[string]bool, error) {
	if canary == "18764" || canary == "18765" {
		return nil, errors.New("kernel assigned a reserved canary port")
	}
	out, err := run("/usr/sbin/lsof", "-nP", "-iTCP", "-sTCP:LISTEN", "-Fpfn")
	if err != nil {
		return nil, err
	}
	listing, ok := strings.CutSuffix(out, "\n")
	if !ok {
		return nil, errors.New("unterminated lsof listing")
	}
	busy := make(map[string]bool)
	var found bool
	var pid string
	var previous byte
	for field := range strings.SplitSeq(listing, "\n") {
		if len(field) < 2 {
			return nil, errors.New("empty lsof field")
		}
		value := field[1:]
		switch field[0] {
		case 'p':
			n, err := strconv.ParseUint(value, 10, 32)
			if (previous != 0 && previous != 'n') || err != nil || n == 0 {
				return nil, errors.New("invalid lsof process record")
			}
			pid = value
		case 'f':
			if _, err := strconv.ParseUint(value, 10, 32); (previous != 'p' && previous != 'n') || err != nil {
				return nil, errors.New("invalid lsof descriptor record")
			}
		case 'n':
			port, ok := endpointPort(value)
			if previous != 'f' || !ok || port == "*" || port == "0" {
				return nil, errors.New("invalid lsof listener endpoint")
			}
			if pid == ownPID && port == canary {
				found = true
			}
			if port == "18764" || port == "18765" {
				busy[port] = true
			}
		default:
			return nil, errors.New("unexpected lsof field")
		}
		previous = field[0]
	}
	if previous != 'n' || !found {
		return nil, errors.New("incomplete listing or missing canary")
	}
	return busy, nil
}
