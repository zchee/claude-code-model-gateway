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

package procinfo

import (
	"context"
	"errors"
	"os"
	"slices"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	gnet "github.com/shirou/gopsutil/v4/net"

	"github.com/zchee/claude-code-model-gateway/internal/venue"
)

func TestMain(m *testing.M) {
	venue.Require()
	os.Exit(m.Run())
}

func TestProbeTimeout(t *testing.T) {
	tests := map[string]struct {
		goos, override string
		want           time.Duration
	}{
		"windows_default":   {"windows", "", 8 * time.Second},
		"linux_default":     {"linux", "", 2 * time.Second},
		"darwin_default":    {"darwin", "", 2 * time.Second},
		"windows_override":  {"windows", "5000", 5 * time.Second},
		"linux_override":    {"linux", "5000", 5 * time.Second},
		"negative_override": {"windows", "-5", time.Millisecond},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("CODEX_GATEWAY_PROBE_TIMEOUT_MS", tt.override)
			if got := ProbeTimeout(tt.goos); got != tt.want {
				t.Errorf("ProbeTimeout(%q) with %q = %s, want %s", tt.goos, tt.override, got, tt.want)
			}
		})
	}
}

func TestIdentityEvidence(t *testing.T) {
	live := Identity{PID: 702, ParentPID: 1, Argv: []string{"node", "fixture"}, Birth: time.Unix(100, 0)}
	tests := map[string]struct {
		info                          Identity
		inspectErr, existErr, wantErr error
		want                          Identity
		wantProbes                    []int
	}{
		"live_but_unreadable": {inspectErr: syscall.EPERM, wantErr: ErrUnknown, wantProbes: []int{702}},
		"denied":              {inspectErr: syscall.EPERM, existErr: syscall.EPERM, wantErr: ErrUnknown, wantProbes: []int{702}},
		"missing":             {inspectErr: os.ErrNotExist, existErr: syscall.ESRCH, wantErr: os.ErrNotExist, wantProbes: []int{702}},
		"readable":            {info: live, want: live},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var inspected, checked []int
			p := Probe{inspect: func(_ context.Context, pid int) (Identity, error) {
				inspected = append(inspected, pid)
				return tt.info, tt.inspectErr
			}, existence: func(_ context.Context, pid int) error { checked = append(checked, pid); return tt.existErr }}
			got, err := p.Identity(t.Context(), 702)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Identity error = %v, want %v", err, tt.wantErr)
			}
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("identity (-want +got): %s", diff)
			}
			if !slices.Equal(inspected, []int{702}) || !slices.Equal(checked, tt.wantProbes) {
				t.Errorf("inspect/existence called for %v/%v, want [702]/%v", inspected, checked, tt.wantProbes)
			}
		})
	}
}

func TestMissingPID(t *testing.T) {
	p := Probe{existence: func(context.Context, int) error {
		t.Error("missing pid reached the system existence probe")
		return nil
	}}
	var missing int
	got, err := p.Alive(t.Context(), missing)
	if got || err != nil {
		t.Errorf("absent pid = %t, %v; want false, nil", got, err)
	}
}

func TestProcessTable(t *testing.T) {
	root := Identity{PID: 1, Argv: []string{"init"}, Birth: time.Unix(100, 0)}
	child := Identity{PID: 702, ParentPID: 1, Argv: []string{"node", "fixture"}, Birth: time.Unix(200, 0)}
	tests := map[string]struct {
		rows     []Identity
		queryErr error
		want     map[int]Identity
	}{
		"root_and_child": {rows: []Identity{root, child}, want: map[int]Identity{1: root, 702: child}},
		"failed_query":   {rows: []Identity{child}, queryErr: syscall.EIO},
		"timeout":        {rows: []Identity{child}, queryErr: context.DeadlineExceeded},
		"malformed":      {rows: []Identity{{}, {PID: -1}, {PID: 702, ParentPID: -1}}, want: map[int]Identity{}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			calls := 0
			p := Probe{processes: func(context.Context) ([]Identity, error) { calls++; return tt.rows, tt.queryErr }}
			got, err := p.Table(t.Context())
			if !errors.Is(err, tt.queryErr) {
				t.Errorf("Table error = %v, want %v", err, tt.queryErr)
			}
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("table (-want +got): %s", diff)
			}
			if calls != 1 {
				t.Errorf("whole-table queries = %d, want 1", calls)
			}
		})
	}
}

func TestListenerEvidence(t *testing.T) {
	const port = 27123
	listen := gnet.ConnectionStat{Status: "LISTEN", Laddr: gnet.Addr{Port: port}, Pid: 71488}
	other := listen
	other.Pid = 9001
	other.Laddr.Port = 28123
	connected := listen
	connected.Status = "ESTABLISHED"
	connected.Pid = 9010
	connected.Raddr.Port = 50123
	tests := map[string]struct {
		port uint16
		rows []gnet.ConnectionStat
		err  error
		want Listener
	}{
		"numeric_owner":  {port: port, rows: []gnet.ConnectionStat{other, connected, listen, listen}, want: Listener{ListenerIdentified, 71488}},
		"denied":         {port: port, rows: []gnet.ConnectionStat{listen}, err: syscall.EACCES, want: Listener{State: ListenerDenied}},
		"failed_lookup":  {port: port, rows: []gnet.ConnectionStat{listen}, err: syscall.EIO, want: Listener{State: ListenerUnknown}},
		"missing_port":   {want: Listener{State: ListenerAbsent}},
		"unresolved_pid": {port: port, rows: []gnet.ConnectionStat{{Status: "LISTEN", Laddr: gnet.Addr{Port: port}}}, want: Listener{State: ListenerUnresolved}},
		"vacant":         {port: port, rows: []gnet.ConnectionStat{other, connected}, want: Listener{State: ListenerAbsent}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			p := Probe{connections: func(context.Context, string) ([]gnet.ConnectionStat, error) { return tt.rows, tt.err }}
			if diff := gocmp.Diff(tt.want, p.Listener(t.Context(), tt.port)); diff != "" {
				t.Errorf("listener (-want +got): %s", diff)
			}
		})
	}
}

func TestListenerBudget(t *testing.T) {
	t.Setenv("CODEX_GATEWAY_PROBE_TIMEOUT_MS", "50")
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		p := Probe{connections: func(ctx context.Context, _ string) ([]gnet.ConnectionStat, error) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Second):
				return nil, errors.New("query exceeded its budget")
			}
		}}
		got := p.Listener(t.Context(), 27123)
		if elapsed := time.Since(started); elapsed != 50*time.Millisecond || got != (Listener{State: ListenerTimedOut}) {
			t.Errorf("listener after %s = %+v, want timeout without pid after 50ms", elapsed, got)
		}
	})
}

func TestLegacyStart(t *testing.T) {
	birth := time.Date(2001, time.February, 3, 22, 15, 9, 123456000, time.UTC)
	tests := map[string]struct {
		location *time.Location
		want     string
	}{
		"single_digit_utc":     {time.UTC, "Sat Feb  3 22:15:09 2001"},
		"non_utc_day_boundary": {time.FixedZone("east", 9*3600), "Sun Feb  4 07:15:09 2001"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := LegacyStart(birth.In(tt.location)); got != tt.want {
				t.Errorf("legacy start = %q, want %q", got, tt.want)
			}
		})
	}
}
