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

//go:build darwin && process

package procinfo

import (
	"context"
	"net"
	"os"
	"testing"
	"time"
)

func TestDarwinShortLivedCommand(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err := darwinCommand(ctx, "/usr/bin/true"); err != nil {
		t.Fatal(err)
	}
}

func TestDarwinListenerEvidence(t *testing.T) {
	l := listenerOn(t, net.ListenConfig{}, "127.0.0.1", 0)
	defer func() { _ = l.Close() }()
	port := uint16(l.Addr().(*net.TCPAddr).Port)
	got, err := (Probe{}).ListenerEvidence(t.Context(), port)
	if err != nil || got != (Listener{ListenerIdentified, os.Getpid()}) {
		t.Fatalf("private listener on %d: state=%s pid=%d: %v", port, got.State, got.PID, err)
	}
}
