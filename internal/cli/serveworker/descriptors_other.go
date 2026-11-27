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

package serveworker

import (
	"errors"
	"fmt"
	"net"
	"os"

	"github.com/zchee/claude-code-model-gateway/internal/config"
)

type controlDescriptors struct {
	generation string
	live       *os.File
	listener   net.Listener
	drain      net.Conn
}

// supervisedDescriptors refuses the supervised mode: an inherited listener and drain channel are Unix descriptors.
func supervisedDescriptors(lookup config.LookupEnv) (*controlDescriptors, error) {
	if _, ok := lookup(controlFDsKey); ok {
		return nil, fmt.Errorf("model-gateway: supervised worker descriptors are not supported on this platform: %w", errors.ErrUnsupported)
	}
	return nil, nil
}

func (*controlDescriptors) report(int) error { return errors.ErrUnsupported }

func (*controlDescriptors) close() {}

func fixtureChannel(config.LookupEnv) (*os.File, error) { return nil, nil }

func errorCode(err error) string { return err.Error() }

func acceptQueued(net.Listener, <-chan struct{}) ([]net.Conn, error) {
	return nil, errors.ErrUnsupported
}

func parentEnded(*os.File) bool { return true }
