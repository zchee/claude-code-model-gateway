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

package proxyrecovery_test

import (
	"context"
	json "encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/zchee/claude-code-model-gateway/internal/ownership"
	"github.com/zchee/claude-code-model-gateway/internal/procinfo"
	"github.com/zchee/claude-code-model-gateway/internal/supervisor/proxyrecovery"
)

const (
	adoptedPID = 4242
	proxyExe   = "/home/user/.claude/model-gateway/bin/claude-code-proxy"
	// foreignSupervisorPID is the supervisor of another install that started a proxy from the shared executable.
	foreignSupervisorPID = 4241
)

var (
	proxyBirth = time.Date(2026, time.November, 27, 8, 30, 0, 0, time.UTC)
	proxyImage = procinfo.Image{Path: proxyExe, Device: 64769, Inode: 5151}
)

// proxyIdentity is a live proxy as the native probe samples it on Linux, with parent as its parent process.
func proxyIdentity(pid, parent int) procinfo.Identity {
	return procinfo.Identity{
		PID:        pid,
		ParentPID:  parent,
		Argv:       []string{proxyExe, "serve", "--no-monitor"},
		Birth:      proxyBirth,
		Image:      procinfo.ImageEvidence{Image: proxyImage, DirectFromExecutable: true},
		StartToken: procinfo.StartToken(fmt.Sprintf("2b1c0f9e-6d1a-4c0b-9a51-1f2e3d4c5b6a/%d", 770000+pid)),
	}
}

// unrelatedIdentity is a program that is not a proxy at all, holding the proxy port.
func unrelatedIdentity(pid int) procinfo.Identity {
	return procinfo.Identity{
		PID:        pid,
		ParentPID:  1,
		Argv:       []string{"/usr/local/bin/unrelated-server", "--listen"},
		Birth:      proxyBirth,
		Image:      procinfo.ImageEvidence{Image: procinfo.Image{Path: "/usr/local/bin/unrelated-server", Device: 64769, Inode: 7373}, DirectFromExecutable: true},
		StartToken: procinfo.StartToken(fmt.Sprintf("2b1c0f9e-6d1a-4c0b-9a51-1f2e3d4c5b6a/%d", 770000+pid)),
	}
}

// foreignSupervisor is the Node supervisor of another install, the parent of a proxy it started.
func foreignSupervisor() procinfo.Identity {
	script := "/home/other/.claude/model-gateway/bin/model-gateway.js"
	return procinfo.Identity{
		PID:        foreignSupervisorPID,
		ParentPID:  1,
		Argv:       []string{"/usr/bin/node", script, "serve-shim"},
		Birth:      proxyBirth,
		Image:      procinfo.ImageEvidence{Image: procinfo.Image{Path: "/usr/bin/node", Device: 64769, Inode: 2929}, DirectFromExecutable: true},
		StartToken: procinfo.StartToken(fmt.Sprintf("2b1c0f9e-6d1a-4c0b-9a51-1f2e3d4c5b6a/%d", 770000+foreignSupervisorPID)),
	}
}

// fullRecord is the record a publisher in root writes for the sampled proxy, with every field an authoritative
// record carries.
func fullRecord(root string, id procinfo.Identity) procinfo.Record {
	return procinfo.Record{
		PID:        id.PID,
		Command:    strings.Join(id.Argv, " "),
		StartedAt:  procinfo.LegacyStart(id.Birth),
		Role:       "proxy",
		StateRoot:  root,
		Generation: "proxy-generation-1",
		Port:       proxyPort,
		Version:    "1.2.3",
		Image:      id.Image.Image,
		StartToken: id.StartToken,
	}
}

// host is a fake process table and listener table that ownership.Authority reads, and the signal delivery it uses.
// A process ends at the first signal it receives, and the port it held is released with it.
type host struct {
	mu      sync.Mutex
	procs   map[int]*procinfo.Identity
	exited  map[int]bool
	holders []int
	signals []string
}

// newHost makes a host whose port the holders listen on; others are further live processes that hold no port.
func newHost(holders, others []procinfo.Identity) *host {
	h := &host{procs: map[int]*procinfo.Identity{}, exited: map[int]bool{}}
	for _, id := range holders {
		h.procs[id.PID] = &id
		h.holders = append(h.holders, id.PID)
	}
	for _, id := range others {
		h.procs[id.PID] = &id
	}
	return h
}

func (h *host) Identity(_ context.Context, pid int) (procinfo.Identity, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	id, ok := h.procs[pid]
	if !ok || h.exited[pid] {
		return procinfo.Identity{}, os.ErrNotExist
	}
	sample := *id
	sample.Argv = append([]string(nil), id.Argv...)
	return sample, nil
}

func (h *host) Table(context.Context) (map[int]procinfo.Identity, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	table := map[int]procinfo.Identity{}
	for pid, id := range h.procs {
		if !h.exited[pid] {
			table[pid] = *id
		}
	}
	return table, nil
}

func (h *host) Alive(_ context.Context, pid int) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.procs[pid]
	return ok && !h.exited[pid], nil
}

func (h *host) live() []int {
	var live []int
	for _, pid := range h.holders {
		if !h.exited[pid] {
			live = append(live, pid)
		}
	}
	return live
}

func (h *host) ListenerEvidence(_ context.Context, port uint16) (procinfo.Listener, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	live := h.live()
	switch {
	case port != proxyPort || len(live) == 0:
		return procinfo.Listener{State: procinfo.ListenerAbsent}, nil
	case len(live) > 1:
		return procinfo.Listener{State: procinfo.ListenerAmbiguous}, nil
	}
	return procinfo.Listener{State: procinfo.ListenerIdentified, PID: live[0]}, nil
}

func (h *host) signal(_ context.Context, id procinfo.Identity, sig os.Signal) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.signals = append(h.signals, fmt.Sprintf("%d %v", id.PID, sig))
	h.exited[id.PID] = true
	return nil
}

func (h *host) listening() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.live()) > 0
}

func (h *host) signalled() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.signals...)
}

// adoption is one arrangement of the proxy port's holder and this state root's records.
type adoption struct {
	// holders are the processes on the proxy port; others are live processes beside them.
	holders, others []procinfo.Identity
	// record, when set, returns the proxy record to seed and the root it goes into; ours is this monitor's root and
	// other is another instance's.
	record func(ours, other string) (root string, r procinfo.Record)
	// raw writes the record's bytes into the root's directory directly, as no publisher would: the record writer
	// refuses a record whose state-root field names another root.
	raw bool
	// class is what this root's authority must find the holder to be.
	class ownership.Class
}

// adoptions are the arrangements the adoption tests share. Each negative differs from the adoptable one in one
// respect.
var adoptions = map[string]adoption{
	"success: a holder that is not a descendant, with this root's full record": {
		holders: []procinfo.Identity{proxyIdentity(adoptedPID, 1)},
		record: func(ours, _ string) (string, procinfo.Record) {
			return ours, fullRecord(ours, proxyIdentity(adoptedPID, 1))
		},
		class: ownership.Ours,
	},
	"error: the record has no start token": {
		holders: []procinfo.Identity{proxyIdentity(adoptedPID, 1)},
		record: func(ours, _ string) (string, procinfo.Record) {
			r := fullRecord(ours, proxyIdentity(adoptedPID, 1))
			r.StartToken = ""
			return ours, r
		},
		class: ownership.Unknown,
	},
	"error: the record has another start token": {
		holders: []procinfo.Identity{proxyIdentity(adoptedPID, 1)},
		record: func(ours, _ string) (string, procinfo.Record) {
			r := fullRecord(ours, proxyIdentity(adoptedPID, 1))
			r.StartToken = proxyIdentity(adoptedPID+1, 1).StartToken
			return ours, r
		},
		class: ownership.Unknown,
	},
	"error: no record names the holder": {
		holders: []procinfo.Identity{proxyIdentity(adoptedPID, 1)},
		class:   ownership.Unknown,
	},
	"error: the record names an earlier process": {
		holders: []procinfo.Identity{proxyIdentity(adoptedPID, 1)},
		record: func(ours, _ string) (string, procinfo.Record) {
			return ours, fullRecord(ours, proxyIdentity(adoptedPID-1, 1))
		},
		class: ownership.Unknown,
	},
	"error: the record is another state root's": {
		holders: []procinfo.Identity{proxyIdentity(adoptedPID, 1)},
		record: func(_, other string) (string, procinfo.Record) {
			return other, fullRecord(other, proxyIdentity(adoptedPID, 1))
		},
		class: ownership.Unknown,
	},
	"error: the record is for another port": {
		holders: []procinfo.Identity{proxyIdentity(adoptedPID, 1)},
		record: func(ours, _ string) (string, procinfo.Record) {
			r := fullRecord(ours, proxyIdentity(adoptedPID, 1))
			r.Port = proxyPort + 1
			return ours, r
		},
		class: ownership.Unknown,
	},
	"error: the port has two holders": {
		holders: []procinfo.Identity{proxyIdentity(adoptedPID, 1), proxyIdentity(adoptedPID+1, 1)},
		record: func(ours, _ string) (string, procinfo.Record) {
			return ours, fullRecord(ours, proxyIdentity(adoptedPID, 1))
		},
		class: ownership.Unknown,
	},
	"error: a descendant of this supervisor without a record": {
		holders: []procinfo.Identity{proxyIdentity(adoptedPID, os.Getpid())},
		class:   ownership.Unknown,
	},
	"error: an unrelated program holds the port": {
		holders: []procinfo.Identity{unrelatedIdentity(adoptedPID)},
		class:   ownership.Unknown,
	},
	"error: the shared proxy of another install's supervisor holds the port": {
		holders: []procinfo.Identity{proxyIdentity(adoptedPID, foreignSupervisorPID)},
		others:  []procinfo.Identity{foreignSupervisor()},
		class:   ownership.Unknown,
	},
	"error: this root's record file names another state root": {
		holders: []procinfo.Identity{proxyIdentity(adoptedPID, 1)},
		record: func(ours, other string) (string, procinfo.Record) {
			return ours, fullRecord(other, proxyIdentity(adoptedPID, 1))
		},
		raw:   true,
		class: ownership.Unknown,
	},
	"error: another root's record file names this state root": {
		holders: []procinfo.Identity{proxyIdentity(adoptedPID, 1)},
		record: func(ours, other string) (string, procinfo.Record) {
			return other, fullRecord(ours, proxyIdentity(adoptedPID, 1))
		},
		raw:   true,
		class: ownership.Unknown,
	},
}

// writeRaw writes r as the proxy record of root, and its decimal mirror, without the record writer's checks.
func writeRaw(t *testing.T, root string, r procinfo.Record) {
	t.Helper()
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("encode the record: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "proxy.pid.json"), data, 0o600); err != nil {
		t.Fatalf("write the record into %s: %v", root, err)
	}
	if err := os.WriteFile(filepath.Join(root, "proxy.pid"), []byte(strconv.Itoa(r.PID)), 0o600); err != nil {
		t.Fatalf("write the record's mirror into %s: %v", root, err)
	}
}

// arrange builds the host and this root's authority for a, with the record seeded by the existing record writer.
func arrange(t *testing.T, a adoption) (*host, ownership.Authority) {
	t.Helper()
	ours, other := t.TempDir(), t.TempDir()
	if a.record != nil {
		root, r := a.record(ours, other)
		if a.raw {
			writeRaw(t, root, r)
		} else if err := procinfo.WriteRecord(root, "proxy", r); err != nil {
			t.Fatalf("seed the proxy record in %s: %v", root, err)
		}
	}
	h := newHost(a.holders, a.others)
	return h, ownership.Authority{Records: ownership.Store{Root: ours}, Processes: h, Signal: h.signal}
}

// TestAdoptionFixtures checks the arrangements without a monitor: this root's authority classifies each holder as the
// case relies on, and the adoptable holder is ended by the authority's stop with the fake delivery.
func TestAdoptionFixtures(t *testing.T) {
	for name, a := range adoptions {
		t.Run(name, func(t *testing.T) {
			bubble(t, func(t *testing.T) {
				h, auth := arrange(t, a)
				owner, err := auth.Resolve(t.Context(), "proxy", proxyPort)
				if err != nil || owner.Class != a.class {
					t.Fatalf("Resolve = %+v, %v; want class %q", owner, err, a.class)
				}
				if a.class != ownership.Ours {
					return
				}
				r, err := auth.Records.Read("proxy")
				if err != nil {
					t.Fatalf("read the seeded record: %v", err)
				}
				if err := auth.Stop(t.Context(), "proxy", r); err != nil {
					t.Fatalf("Stop the adoptable holder: %v", err)
				}
				if diff := gocmp.Diff([]string{fmt.Sprintf("%d %v", adoptedPID, syscall.SIGTERM)}, h.signalled()); diff != "" {
					t.Errorf("signals (-want +got):\n%s", diff)
				}
				if h.listening() {
					t.Error("the port is still held after the stop")
				}
			})
		})
	}
}

// adopt wires a world's ownership collaborators to this root's authority and to the host's port.
func adopt(w *world, h *host, auth ownership.Authority) {
	w.set(func(b *behaviour) {
		b.listening = func(context.Context) (bool, error) { return h.listening(), nil }
		b.resolve = func(ctx context.Context) (ownership.Owner, error) { return auth.Resolve(ctx, "proxy", proxyPort) }
		b.recorded = func(_ context.Context, pid int) (procinfo.Record, error) {
			r, err := auth.Records.Read("proxy")
			if err != nil {
				return procinfo.Record{}, err
			}
			if r.PID != pid {
				return procinfo.Record{}, errNoRecord
			}
			return r, nil
		}
		// A proxy this monitor started is the world's child; any other holder is stopped by this root's authority.
		b.stopOwned = func(ctx context.Context, r procinfo.Record) error {
			if c := w.child(r.PID); c != nil {
				c.exit()
				return nil
			}
			return auth.Stop(ctx, "proxy", r)
		}
		b.waitRelease = func(context.Context) bool { return !h.listening() }
	})
}

// TestRecordedProxyAdoption decides when an unhealthy proxy that this monitor did not start is replaced. Only a live
// listener that this state root's full record names, start token included, is stopped, and then replaced; ancestry
// without such a record, a record that is missing, stale, another root's, for another port or without the holder's
// start token, a record whose state-root field disagrees with its directory, an unrelated program, the shared proxy
// of another install's supervisor, several holders, and the unmanaged mode all leave it running and start nothing:
// the monitor does not even ask for a stop, and a managed monitor looks the holder up again at its retry time. A
// holder that answers is never touched, whatever its record.
func TestRecordedProxyAdoption(t *testing.T) {
	type expectation struct {
		managed bool
		// healthy makes the holder answer its probes.
		healthy bool
		state   proxyrecovery.State
		signals []string
		started []int
	}
	adopted := []string{fmt.Sprintf("%d %v", adoptedPID, syscall.SIGTERM)}
	tests := map[string]struct {
		arrangement string
		expectation
	}{}
	for name, a := range adoptions {
		e := expectation{managed: true, state: proxyrecovery.OwnerUnknown}
		if a.class == ownership.Ours {
			e = expectation{managed: true, state: proxyrecovery.Recovered, signals: adopted, started: []int{900}}
		}
		tests[name] = struct {
			arrangement string
			expectation
		}{name, e}
	}
	adoptable := "success: a holder that is not a descendant, with this root's full record"
	tests["error: the unmanaged mode with this root's full record"] = struct {
		arrangement string
		expectation
	}{adoptable, expectation{state: proxyrecovery.ProxyDown}}
	tests["success: a healthy holder with this root's full record is left running"] = struct {
		arrangement string
		expectation
	}{adoptable, expectation{managed: true, healthy: true, state: proxyrecovery.Healthy}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			bubble(t, func(t *testing.T) {
				h, auth := arrange(t, adoptions[tt.arrangement])
				w := newWorld(t)
				adopt(w, h, auth)
				// The holder never answers; a proxy this monitor starts answers at once.
				w.set(func(b *behaviour) {
					b.probe = func(context.Context) bool { return tt.healthy || len(w.startedPIDs()) > 0 }
				})
				m := newMonitor(t, w.options(tt.managed))
				res := tick(t, m)
				if res.State != tt.state {
					t.Errorf("state = %q, want %q; calls %q", res.State, tt.state, w.sequence())
				}
				if diff := gocmp.Diff(tt.signals, h.signalled(), cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("signals (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff(tt.started, w.startedPIDs(), cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("proxies started (-want +got):\n%s", diff)
				}
				if tt.state == proxyrecovery.Recovered {
					var stopRequested []event
					for _, e := range w.journal() {
						if e.Name == "proxy-stop-requested" {
							stopRequested = append(stopRequested, e)
						}
					}
					if diff := gocmp.Diff([]event{stopRequestedEvent(adoptedPID)}, stopRequested); diff != "" {
						t.Errorf("stop journal (-want +got):\n%s", diff)
					}
				}
				if tt.managed && tt.state == proxyrecovery.OwnerUnknown {
					w.until(res.RetryAt)
					if again := tick(t, m); again.State != proxyrecovery.OwnerUnknown {
						t.Errorf("the tick at the retry time = %+v, want owner-unknown", again)
					}
					if n := w.count("resolve"); n != 2 {
						t.Errorf("the holder was looked up %d times in two attempts, want 2", n)
					}
				}
				if tt.state != proxyrecovery.Recovered {
					if n := w.count("stop-owned") + w.count("start"); n != 0 {
						t.Errorf("a refused holder led to %d stops or starts", n)
					}
				}
				if err := stop(t, m); err != nil {
					t.Errorf("Stop: %v", err)
				}
				if tt.state != proxyrecovery.Recovered {
					if diff := gocmp.Diff([]string(nil), h.signalled(), cmpopts.EquateEmpty()); diff != "" {
						t.Errorf("signals after Stop (-want +got):\n%s", diff)
					}
				}
			})
		})
	}
}
