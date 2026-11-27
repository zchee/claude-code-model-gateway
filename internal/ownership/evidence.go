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

package ownership

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/procinfo"
)

// The roles a record names, and how a live command line shows each. The gateway CLI's command is its first argument
// (internal/cli/root.go), so the supervisor and the request worker are told apart by that word alone; a word that
// appears later is an operand and proves nothing. The proxy is its own binary.
const (
	roleSupervisor = "supervisor"
	roleWorker     = "worker"
	roleProxy      = "proxy"
	proxyBinary    = "claude-code-proxy"
)

var gatewayCommands = map[string]string{roleSupervisor: "serve-shim", roleWorker: "serve-worker"}

// runsRole reports whether argv shows the process running role. An unknown role is never shown.
func runsRole(role string, argv []string) bool {
	if role == roleProxy {
		return len(argv) > 0 && filepath.Base(argv[0]) == proxyBinary
	}
	command, ok := gatewayCommands[role]
	return ok && len(argv) > 1 && argv[1] == command
}

// listenerDecides reports whether the record's port must be held by the recorded process alone. The supervisor's
// public port and the proxy's port have one holder; the worker's private port is legitimately held by the supervisor
// as well, so it decides nothing.
func listenerDecides(role string) bool { return role == roleSupervisor || role == roleProxy }

// contradiction returns why a live sample of the recorded number does not show the recorded process, or the empty
// string when every field agrees: a start token on both sides that is equal, the birth, the command line and the
// role it shows, and the executable's device and inode when the probe read them through the kernel's own reference
// to the executable. Correlated image evidence (as macOS supplies) neither proves nor contradicts, and the image path
// never decides: another spelling of the recorded executable with its device and inode is the same image. The port
// is not judged here.
func contradiction(r procinfo.Record, id procinfo.Identity) string {
	switch {
	case id.PID != r.PID:
		return "the sample is of another process number"
	case r.StartToken == "":
		return "the record has no start token"
	case id.StartToken == "":
		return "the live process has no readable start token"
	case id.StartToken != r.StartToken:
		return "the live process has another start token"
	case procinfo.LegacyStart(id.Birth) != r.StartedAt:
		return "the live process has another birth"
	case strings.Join(id.Argv, " ") != r.Command:
		return "the live process runs another command line"
	case !runsRole(r.Role, id.Argv):
		return "the live command line does not run the recorded role"
	case id.Image.DirectFromExecutable && (id.Image.Device != r.Image.Device || id.Image.Inode != r.Image.Inode):
		return "the live process runs another executable"
	}
	return ""
}

// defaultProbe is the one native probe every Authority without its own Processes shares, so that its single
// outstanding listener query slot bounds native queries across calls: a query abandoned at a deadline keeps the slot
// until it finishes, and later queries are refused as unknown instead of piling up.
var defaultProbe = sync.OnceValue(func() procinfo.Probe { return procinfo.New(nil) })

func (a Authority) processes() Processes {
	if a.Processes != nil {
		return a.Processes
	}
	return defaultProbe()
}

func (a Authority) wait(ctx context.Context, d time.Duration) error {
	if a.Wait != nil {
		return a.Wait(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
