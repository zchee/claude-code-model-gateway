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

//go:build unix

package relay_test

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"testing"

	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// realLinks is the real workerlink.Registry with a listener that never accepts and the world's in-memory dials. Its
// reservations open a real drain socket pair, which is why it is used only here.
type realLinks struct {
	*workerlink.Registry
}

func newRealLinks(w *world) links {
	return realLinks{workerlink.NewRegistry(listenFake, w.dial)}
}

func (l realLinks) reserve(ctx context.Context, generation workerlink.Generation, port uint16) (reservation, error) {
	res, err := l.Reserve(ctx, generation, port)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// TestRealRegistry runs representative delivery cases through the real workerlink.Registry, so that the in-memory
// links the other tests use are checked against it: delivery and resend after an end, the still-current taker, the
// hand-over of a dial after retirement, the cancellation of a dial by an abrupt end and the close of one that completes
// anyway, the retired but live taker, the last-attempt exception and a successor on the same port.
func TestRealRegistry(t *testing.T) {
	cases := deliveryCases()
	for _, name := range []string{
		"success: the request is sent again after the generation that took it ended",
		"error: a generation that is still current is not sent the request again",
		"success: a dial that completes after its generation was retired is a delivery to it",
		"success: a dial cancelled by the abrupt end of its generation is not a delivery",
		"error: a connection that took the request on the last attempt and failed is answered as taken although its generation ended",
		"success: a successor on the same port is a new generation and gets the request",
		"success: each newer generation gets the request after the one that took it ended",
		"error: a retired generation that is still alive keeps the request as taken",
		"success: a dial that completes after the abrupt end of its generation is not a delivery",
	} {
		tt, ok := cases[name]
		if !ok {
			t.Fatalf("no delivery case %q", name)
		}
		t.Run(name, func(t *testing.T) {
			inBubbleWith(t, newRealLinks, func(t *testing.T, w *world) { runDeliveryCase(t, w, tt) })
		})
	}
}

// listenFake binds a fake listener at the requested loopback port; the reservation never accepts on it.
func listenFake(_ context.Context, _, address string) (net.Listener, error) {
	_, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return nil, fmt.Errorf("the fake listener needs a fixed port, not %q", address)
	}
	return &fakeListener{addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(port)}, closed: make(chan struct{})}, nil
}

// fakeListener is the listener a reservation holds; the relay path never accepts on it.
type fakeListener struct {
	addr   net.Addr
	once   sync.Once
	closed chan struct{}
}

func (l *fakeListener) Accept() (net.Conn, error) {
	<-l.closed
	return nil, net.ErrClosed
}

func (l *fakeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *fakeListener) Addr() net.Addr { return l.addr }
