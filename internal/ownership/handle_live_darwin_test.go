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

//go:build process && darwin

package ownership

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// handleTReferencePath is the import path of the package itself; a copy of it never forwards.
const handleTReferencePath = "github.com/zchee/claude-code-model-gateway/internal/ownership"

func handleTIsReference() bool { return reflect.TypeFor[Handle]().PkgPath() == handleTReferencePath }

// HandleTFixture checks, without the package, that the held child can be asked with kill of signal 0, which delivers
// nothing, and returns the only backend macOS has. In a copy of the package it refuses before making any call.
func HandleTFixture(held int) (string, error) {
	if !handleTIsReference() {
		return "", errors.New("not the package itself: the live rows run with the reference alone")
	}
	if err := unix.Kill(held, 0); err != nil {
		return "", fmt.Errorf("kill of the held child with signal 0: %w", err)
	}
	return "number", nil
}

// HandleTGuard confines kill, the only delivery call of this platform, to the held child of one live row from
// HandleTHoldChild until Stop: while it forwards it makes a real kill only of that child's number with SIGTERM. Every
// other call, and every call after Stop, is recorded as refused and answered EPERM without being made. A copy of the
// package never forwards.
type HandleTGuard struct {
	held int

	mu         sync.Mutex
	forwarding bool
	trace      []string
}

// HandleTHoldChild installs the guard for held until the row ends; the row calls Stop before it reaps the child.
func HandleTHoldChild(t *testing.T, held int) *HandleTGuard {
	t.Helper()
	g := &HandleTGuard{held: held, forwarding: handleTIsReference()}
	signal := kill
	kill = g.kill
	t.Cleanup(func() {
		g.Stop()
		kill = signal
	})
	return g
}

func (g *HandleTGuard) kill(pid int, sig syscall.Signal) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	who := "other"
	if pid == g.held {
		who = "held"
	}
	call := fmt.Sprintf("kill(%s, %v)", who, sig)
	if !g.forwarding || pid != g.held || sig != syscall.SIGTERM {
		g.trace = append(g.trace, "refused "+call)
		return unix.EPERM
	}
	err := unix.Kill(g.held, syscall.SIGTERM)
	if err != nil {
		call += ": " + err.Error()
	}
	g.trace = append(g.trace, call)
	return err
}

// Stop ends the forwarding, from here on every call is refused, and returns the calls traced. It is idempotent.
func (g *HandleTGuard) Stop() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.forwarding = false
	return slices.Clone(g.trace)
}
