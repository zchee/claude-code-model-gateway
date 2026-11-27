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

package relay_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"sync"

	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// memLinks is an in-memory workerlink.Source with the admission rules of workerlink.Registry, without its listener and
// drain channel, so that a synctest bubble holds no real socket:
//   - Acquire answers at once when no generation is eligible: ErrRetired while a retired generation other than the
//     excluded one still lives, ErrUnavailable otherwise; a context that has already ended gives its error;
//   - at most one generation is eligible; Publish refuses a pid of zero or less, a retired or ended generation, a
//     process identifier other than the one already published (the same one is accepted again) and a second eligible
//     generation;
//   - an admitted dial pins its generation: Retire leaves the pin and hands a successful dial over; End cancels the
//     dial, closes a connection that completes anyway, closes Exited and waits for the pins.
type memLinks struct {
	dial func(ctx context.Context, generation workerlink.Generation) (net.Conn, error)

	mu       sync.Mutex
	live     []*memRes
	eligible *memRes
}

func newMemLinks(w *world) links { return &memLinks{dial: w.dialGeneration} }

// memRes is the reservation of one generation in memLinks.
type memRes struct {
	links      *memLinks
	generation workerlink.Generation
	port       uint16
	endCtx     context.Context
	endCancel  context.CancelFunc

	// The fields below are guarded by links.mu.
	pid         int
	retired     bool
	ended       bool
	pins        int
	pinsSettled chan struct{}
	endOnce     sync.Once
}

func (l *memLinks) reserve(ctx context.Context, generation workerlink.Generation, port uint16) (reservation, error) {
	if generation == "" || port == 0 {
		return nil, fmt.Errorf("memLinks: generation %q at port %d cannot be reserved", generation, port)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if slices.ContainsFunc(l.live, func(r *memRes) bool { return r.generation == generation }) {
		return nil, fmt.Errorf("memLinks: generation %q is already reserved", generation)
	}
	// The generation outlives the caller's context but keeps its values, as the registry's reservation does.
	endCtx, endCancel := context.WithCancel(context.WithoutCancel(ctx))
	r := &memRes{links: l, generation: generation, port: port, endCtx: endCtx, endCancel: endCancel}
	l.live = append(l.live, r)
	return r, nil
}

func (l *memLinks) Acquire(ctx context.Context, exclude workerlink.Generation) (workerlink.Lease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	r := l.eligible
	if r == nil || r.generation == exclude {
		err := workerlink.ErrUnavailable
		if slices.ContainsFunc(l.live, func(o *memRes) bool { return o.retired && o.generation != exclude }) {
			err = workerlink.ErrRetired
		}
		l.mu.Unlock()
		return nil, err
	}
	if r.pins == 0 {
		r.pinsSettled = make(chan struct{})
	}
	r.pins++
	l.mu.Unlock()

	dialCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(r.endCtx, cancel)
	conn, err := l.dial(dialCtx, r.generation)
	stop()
	cancel()

	// A successful hand-over is one critical section, as in the registry: the check, the lease with the published
	// process identifier and the release of the pin.
	l.mu.Lock()
	if err == nil && !r.ended {
		lease := &memLease{Conn: conn, res: r, pid: r.pid}
		r.unpinLocked()
		l.mu.Unlock()
		return lease, nil
	}
	l.mu.Unlock()
	// A connection that completed after End is closed before the pin is released, so that End never returns with it
	// open.
	if err == nil {
		_ = conn.Close()
	}
	l.mu.Lock()
	r.unpinLocked()
	l.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("memLinks: connect to generation %q: %w", r.generation, err)
	}
	return nil, fmt.Errorf("memLinks: generation %q ended while its connection was opened: %w", r.generation, workerlink.ErrUnavailable)
}

// unpinLocked settles one admitted dial and wakes End when it was the last.
func (r *memRes) unpinLocked() {
	r.pins--
	if r.pins == 0 {
		close(r.pinsSettled)
	}
}

func (r *memRes) Port() uint16 { return r.port }

func (r *memRes) Publish(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("memLinks: generation %q cannot be published for process %d", r.generation, pid)
	}
	l := r.links
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case r.ended:
		return fmt.Errorf("memLinks: generation %q has ended: %w", r.generation, workerlink.ErrUnavailable)
	case r.retired:
		return fmt.Errorf("memLinks: generation %q cannot be published: %w", r.generation, workerlink.ErrRetired)
	case r.pid != 0 && r.pid != pid:
		return fmt.Errorf("memLinks: generation %q is already published for process %d", r.generation, r.pid)
	case l.eligible != nil && l.eligible != r:
		return errors.New("memLinks: another generation is eligible")
	}
	r.pid = pid
	l.eligible = r
	return nil
}

func (r *memRes) Retire() {
	l := r.links
	l.mu.Lock()
	defer l.mu.Unlock()
	if r.ended {
		return
	}
	r.retired = true
	if l.eligible == r {
		l.eligible = nil
	}
}

func (r *memRes) End() error {
	r.endOnce.Do(func() {
		l := r.links
		l.mu.Lock()
		r.ended = true
		if l.eligible == r {
			l.eligible = nil
		}
		l.live = slices.DeleteFunc(l.live, func(o *memRes) bool { return o == r })
		l.mu.Unlock()
		r.endCancel()
		for {
			l.mu.Lock()
			if r.pins == 0 {
				l.mu.Unlock()
				return
			}
			settled := r.pinsSettled
			l.mu.Unlock()
			<-settled
		}
	})
	return nil
}

// memLease is a connection handed over for one generation of memLinks.
type memLease struct {
	net.Conn
	res *memRes
	pid int
}

func (l *memLease) Generation() workerlink.Generation { return l.res.generation }

func (l *memLease) PID() int { return l.pid }

func (l *memLease) Authority() string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(int(l.res.port)))
}

func (l *memLease) Exited() <-chan struct{} { return l.res.endCtx.Done() }
