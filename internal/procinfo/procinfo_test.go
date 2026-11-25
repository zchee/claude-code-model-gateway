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

	"github.com/zchee/claude-code-model-gateway/internal/venue"
)

func TestMain(m *testing.M) {
	venue.Require()
	os.Exit(m.Run())
}

func TestProbeTimeout(t *testing.T) {
	tests := map[string]struct {
		goos     string
		override *string
		want     time.Duration
	}{
		"windows_default":   {"windows", nil, 8 * time.Second},
		"linux_default":     {"linux", nil, 2 * time.Second},
		"darwin_default":    {"darwin", nil, 2 * time.Second},
		"empty_override":    {"linux", new(""), 2 * time.Second},
		"windows_override":  {"windows", new("5000"), 5 * time.Second},
		"linux_override":    {"linux", new("5000"), 5 * time.Second},
		"negative_override": {"windows", new("-5"), time.Millisecond},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			const key = "CODEX_GATEWAY_PROBE_TIMEOUT_MS"
			if tt.override != nil {
				t.Setenv(key, *tt.override)
			} else {
				t.Setenv(key, "") // Register cleanup for the original environment before unsetting it.
				if err := os.Unsetenv(key); err != nil {
					t.Fatal(err)
				}
			}
			if got := ProbeTimeout(tt.goos); got != tt.want {
				t.Errorf("ProbeTimeout(%q) = %s, want %s", tt.goos, got, tt.want)
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
		"live_but_unreadable":       {inspectErr: syscall.EPERM, wantErr: ErrUnknown, wantProbes: []int{702}},
		"denied":                    {inspectErr: syscall.EPERM, existErr: syscall.EPERM, wantErr: ErrUnknown, wantProbes: []int{702}},
		"missing":                   {inspectErr: os.ErrNotExist, existErr: syscall.ESRCH, wantErr: os.ErrNotExist, wantProbes: []int{702}},
		"missing_path_but_alive":    {inspectErr: syscall.ENOENT, wantErr: ErrUnknown, wantProbes: []int{702}},
		"partial_failed_inspection": {info: Identity{PID: 702, Argv: []string{"fixture"}}, inspectErr: syscall.EIO, wantErr: ErrUnknown, wantProbes: []int{702}},
		"readable":                  {info: live, want: live},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var inspected, checked []int
			p := Probe{inspect: func(_ context.Context, pid int) (Identity, error) {
				inspected = append(inspected, pid)
				return tt.info, tt.inspectErr
			}, existence: func(_ context.Context, pid int) error { checked = append(checked, pid); return tt.existErr }}
			got, err := p.Identity(t.Context(), 702)
			if !errors.Is(err, tt.wantErr) || (errors.Is(err, ErrUnknown) && errors.Is(err, os.ErrNotExist)) {
				t.Errorf("Identity error = %v, want exclusively %v", err, tt.wantErr)
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

func TestAliveWhenDenied(t *testing.T) {
	var checked []int
	p := Probe{existence: func(_ context.Context, pid int) error { checked = append(checked, pid); return syscall.EPERM }}
	alive, err := p.Alive(t.Context(), 702)
	if !alive || err != nil || !slices.Equal(checked, []int{702}) {
		t.Errorf("liveness after EPERM = %t, %v; checked %v, want true, nil for pid 702", alive, err, checked)
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
	listen := socketEvidence{port: port, listening: true, owners: []int{71488}}
	other := socketEvidence{port: 28123, listening: true, owners: []int{9001}}
	connected := socketEvidence{port: port, owners: []int{9010}}
	unresolved := socketEvidence{port: port, listening: true}
	tests := map[string]struct {
		port uint16
		rows []socketEvidence
		err  error
		want Listener
	}{
		"numeric_owner":     {port: port, rows: []socketEvidence{other, connected, listen, listen}, want: Listener{ListenerIdentified, 71488}},
		"denied":            {port: port, rows: []socketEvidence{listen}, err: syscall.EACCES, want: Listener{State: ListenerDenied}},
		"failed_lookup":     {port: port, rows: []socketEvidence{listen}, err: syscall.EIO, want: Listener{State: ListenerUnknown}},
		"missing_port":      {rows: []socketEvidence{listen, other}, want: Listener{State: ListenerAbsent}},
		"unresolved_pid":    {port: port, rows: []socketEvidence{unresolved}, want: Listener{State: ListenerUnresolved}},
		"partial_ownership": {port: port, rows: []socketEvidence{listen, unresolved}, want: Listener{State: ListenerAmbiguous}},
		"shared_socket":     {port: port, rows: []socketEvidence{{port: port, listening: true, owners: []int{71488, 9001}}}, want: Listener{State: ListenerAmbiguous}},
		"vacant":            {port: port, rows: []socketEvidence{other, connected}, want: Listener{State: ListenerAbsent}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			p := Probe{sockets: func(context.Context) ([]socketEvidence, error) { return tt.rows, tt.err }}
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
		entered := make(chan struct{}, 1)
		p := Probe{sockets: func(context.Context) ([]socketEvidence, error) {
			entered <- struct{}{}
			// Some kernel reads do not observe context cancellation.
			time.Sleep(time.Second)
			return []socketEvidence{{port: 27123, listening: true, owners: []int{702}}}, nil
		}}
		got := p.Listener(t.Context(), 27123)
		select {
		case <-entered:
		default:
			t.Error("the blocked system query was not started")
		}
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
