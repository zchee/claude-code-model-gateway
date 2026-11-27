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

package workerlink

import (
	"errors"
	"fmt"
	"net"
	"os"
)

// newDrainPair has no local stream socket pair to make on this system, so a reservation cannot be set up and the
// supervised worker path is refused rather than pretended.
func newDrainPair() (parent net.Conn, child *os.File, err error) {
	return nil, nil, fmt.Errorf("a drain channel pair: %w", errors.ErrUnsupported)
}

// importListener closes file and refuses every inherited listener: descriptor inheritance by number is a Unix
// mechanism.
func importListener(file *os.File, _ uint16) (net.Listener, error) {
	_ = file.Close()
	return nil, fmt.Errorf("an inherited worker listener: %w", errors.ErrUnsupported)
}

// importDrainChannel closes file and refuses every inherited drain channel: descriptor inheritance by number is a
// Unix mechanism.
func importDrainChannel(file *os.File) (net.Conn, error) {
	_ = file.Close()
	return nil, fmt.Errorf("an inherited drain channel: %w", errors.ErrUnsupported)
}
