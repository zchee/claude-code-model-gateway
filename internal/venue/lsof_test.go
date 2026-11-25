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
	"io/fs"
	"net"
	"strconv"
	"syscall"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestListenCanary(t *testing.T) {
	tests := map[string]struct {
		busy    int
		other   error
		ports   []int
		success bool
	}{
		"success: first candidate free":              {ports: []int{65535}, success: true},
		"success: first busy then wrap to free port": {busy: 1, ports: []int{65535, 49152}, success: true},
		"error: all candidates busy":                 {busy: 8, ports: []int{65535, 49152, 49153, 49154, 49155, 49156, 49157, 49158}},
		"error: permission failure stops retries":    {other: fs.ErrPermission, ports: []int{65535}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var ports []int
			listener := new(net.TCPListener)
			listen := func(_ context.Context, network, address string) (net.Listener, error) {
				host, port, err := net.SplitHostPort(address)
				if err != nil || network != "tcp4" || host != "127.0.0.1" {
					t.Fatalf("unexpected listen(%q, %q): %v", network, address, err)
				}
				n, err := strconv.Atoi(port)
				if err != nil {
					t.Fatal(err)
				}
				ports = append(ports, n)
				if len(ports) <= tc.busy {
					return nil, &net.OpError{Op: "listen", Err: syscall.EADDRINUSE}
				}
				if tc.other != nil {
					return nil, tc.other
				}
				return listener, nil
			}
			got, err := listenCanary(t.Context(), 65535, listen)
			if diff := gocmp.Diff(tc.ports, ports); diff != "" {
				t.Errorf("attempted ports (-want +got):\n%s", diff)
			}
			if (err == nil) != tc.success || (got == listener) != tc.success || (tc.other != nil && !errors.Is(err, tc.other)) {
				t.Errorf("listener returned=%t, error=%v; want success=%t, other error=%v", got != nil, err, tc.success, tc.other)
			}
		})
	}
}

func TestCanaryPortRange(t *testing.T) {
	ctx := t.Context()
	// Every distinct start, including all retry candidates, must be safe
	// before the injected listen function sees the address.
	for seed := range 1 << 16 {
		calls := 0
		_, err := listenCanary(ctx, uint16(seed), func(_ context.Context, _, address string) (net.Listener, error) {
			calls++
			host, port, splitErr := net.SplitHostPort(address)
			n, parseErr := strconv.Atoi(port)
			if splitErr != nil || parseErr != nil || host != "127.0.0.1" || n < 49152 || n > 65535 {
				t.Fatalf("seed %d attempt %d: invalid bind address %q", seed, calls, address)
			}
			return nil, syscall.EADDRINUSE
		})
		if err == nil || calls != 8 {
			t.Fatalf("seed %d: %d attempts, error %v", seed, calls, err)
		}
	}
}

func TestInspectLsof(t *testing.T) {
	const witness = "p26313\nf4\nn127.0.0.1:5934\n"
	tests := map[string]struct {
		output                string
		runErr                error
		first, second, failed bool
	}{
		"success: canary alone":                        {output: witness},
		"success: IPv4 loopback":                       {output: witness + "p9\nf10\nn127.0.0.1:18764\n", first: true},
		"success: IPv4 wildcard":                       {output: witness + "p9\nf10\nn0.0.0.0:18765\n", second: true},
		"success: IPv6 loopback":                       {output: witness + "p9\nf10\nn[::1]:18765\n", second: true},
		"success: IPv6 wildcard":                       {output: witness + "p9\nf10\nn[::]:18764\n", first: true},
		"success: both wildcard listeners":             {output: witness + "p9\nf10\nn*:18764\nf11\nn*:18765\n", first: true, second: true},
		"error: empty listing":                         {failed: true},
		"error: another PID owns matching port":        {output: "p263130\nf4\nn127.0.0.1:5934\n", failed: true},
		"error: port has same suffix":                  {output: "p26313\nf4\nn127.0.0.1:55934\n", failed: true},
		"error: truncated file after canary":           {output: witness + "f5\n", failed: true},
		"error: missing descriptor":                    {output: witness + "p9\nn*:18764\n", failed: true},
		"error: invalid descriptor":                    {output: witness + "fnot-a-number\nn*:18764\n", failed: true},
		"error: hostname in numeric listing":           {output: witness + "f5\nnlocalhost:18764\n", failed: true},
		"error: remote connection in listener listing": {output: witness + "f5\nn127.0.0.1:5000->127.0.0.1:18764\n", failed: true},
		"error: unknown field after canary":            {output: witness + "TST=UNKNOWN\n", failed: true},
		"error: unterminated line":                     {output: witness + "f5\nn*:18764", failed: true},
		"error: tool failure despite canary":           {output: witness, runErr: fs.ErrPermission, failed: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			busy, err := inspectLsof("26313", "5934", func(string, ...string) (string, error) { return tc.output, tc.runErr })
			if diff := gocmp.Diff([3]bool{tc.first, tc.second, tc.failed}, [3]bool{busy["18764"], busy["18765"], err != nil}); diff != "" {
				t.Errorf("port results and failure (-want +got):\n%s; err=%v", diff, err)
			}
		})
	}
}
