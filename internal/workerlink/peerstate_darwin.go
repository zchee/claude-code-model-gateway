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

package workerlink

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// endedPeerEvidence gathers what XNU reports about a local stream socket whose getpeername failed with EINVAL
// (getpeerErr), for peerEvidence.decide to judge; it always gathers. XNU answers EINVAL for any socket that can neither
// receive nor send (bsd/kern/uipc_syscalls.c:3232-3239). A peer's close leaves a connected stream socket that way: the
// close disconnects the pair (bsd/kern/uipc_usrreq.c:1081-1083), and the disconnect drops the link between the two
// ends (:1684, :1702) and marks both unable to receive or send and no longer connected (:1711-1715). Other sockets
// answer EINVAL as well, so each fact below is gathered:
//
//   - LOCAL_PEERCRED answers once the socket has been connected. A stream socket receives the peer credentials when
//     it is connected, by socketpair (:1567-1574), as the connecting end (:1466-1468) or as the accepted end
//     (:1435-1436); nothing clears them, and the option answers from them alone (:841-851). A socket that has never
//     been connected, a fresh listener included (listen caches only its own credentials, :2770-2776), fails with
//     ENOTCONN.
//   - LOCAL_PEERPID fails with ENOTCONN once the socket is linked to no peer (:853-857). It answers for a socket that
//     can neither receive nor send while still linked to its peer.
//   - The read filter reports EV_EOF for a socket that does not listen and can receive no more. For a listening
//     socket it reports only queued connections and never sets EV_EOF; for any other socket it sets EV_EOF once the
//     socket can receive no more (bsd/kern/uipc_socket.c:6557-6567, :6586-6596), whether or not data is still
//     queued. The content filter that could hold EV_EOF back is attached only by the Internet protocols
//     (bsd/netinet/tcp_usrreq.c:654, :813, tcp_input.c:2563, flow_divert.c:4114; bsd/sys/socketvar.h:1031-1033). This
//     tells a socket that lost its peer and was then made to listen, which XNU permits for a socket bound to a path
//     (bsd/kern/uipc_socket.c:863-866, bsd/kern/uipc_usrreq.c:391-399), and which no socket option reveals (see
//     listeningState).
//   - A receive peek answers with the queued bytes or the end of input. It tells a socket that was made defunct,
//     which keeps the cached credentials and, being shut down both ways (bsd/kern/uipc_socket.c:7583-7585), its
//     end-of-input read filter, but refuses every receive with ENOTCONN before looking at the flags (:3255-3276);
//     recvfrom reaches that receive directly (bsd/kern/uipc_syscalls.c:2232, uipc_usrreq.c:825). See peekReceive for
//     why the peek neither consumes nor waits.
func endedPeerEvidence(fd int, getpeerErr error) (peerEvidence, bool) {
	e := peerEvidence{getpeerErr: getpeerErr}
	e.credAsked = true
	_, e.credErr = unix.GetsockoptXucred(fd, unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	_, e.peerPIDErr = unix.GetsockoptInt(fd, unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	e.readEOF, e.readReport, e.readErr = readFilter(fd)
	e.peekAsked = true
	e.peekN, e.peekErr = peekReceive(fd)
	return e, true
}

// readFilter asks a private kqueue, without waiting, for the read filter's state of fd: whether it reports the end of
// input and, when it does not, what it reports. One kevent call registers the filter and then collects what it
// reports (bsd/kern/kern_event.c:8127-8140, :8209-8218): an attach that finds the event already present marks it
// active (:4143-4146), and collecting it fills in the filter's flags (:4380-4386, bsd/kern/uipc_socket.c:6712-6722,
// kern_event.c:1008-1013). An interrupted call is repeated on the same queue with the same zero timeout; adding the
// same filter again only updates it. XNU creates the kqueue close-on-exec and close-on-fork (:3066); closing it, once,
// drops the filter again (:5095-5105, :2962-3001, :7252-7274).
func readFilter(fd int) (eof bool, report string, err error) {
	kq, err := unix.Kqueue()
	if err != nil {
		return false, "", os.NewSyscallError("kqueue", err)
	}
	defer func() { _ = unix.Close(kq) }()
	changes := make([]unix.Kevent_t, 1)
	unix.SetKevent(&changes[0], fd, unix.EVFILT_READ, unix.EV_ADD)
	events := make([]unix.Kevent_t, 1)
	var n int
	for {
		n, err = unix.Kevent(kq, changes, events, &unix.Timespec{})
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	switch {
	case err != nil:
		return false, "", os.NewSyscallError("kevent", err)
	case n == 0:
		return false, "nothing", nil
	case events[0].Flags&unix.EV_ERROR != 0:
		return false, "", os.NewSyscallError("kevent", unix.Errno(events[0].Data))
	case events[0].Flags&unix.EV_EOF == 0:
		return false, fmt.Sprintf("%d readable, flags %#x", events[0].Data, events[0].Flags), nil
	}
	return true, "", nil
}

// peekReceive receives at most one byte from fd with MSG_PEEK and MSG_DONTWAIT and returns the count: one when bytes
// are queued, zero at the end of input. MSG_PEEK copies the queued bytes and leaves them in place (every removal in
// the receive is guarded by it: bsd/kern/uipc_socket.c:3564, :3698-3700, :3734-3735, :3764, :3866, :3884).
// MSG_DONTWAIT never sleeps: the receive buffer's lock is taken without waiting (:1843, uipc_socket2.c:2529-2530), a
// queued byte is returned at once (uipc_socket.c:3135-3160), an empty buffer of a socket that can receive no more
// returns zero (:3444-3459), and an empty buffer of any other socket fails with EWOULDBLOCK instead of waiting
// (:3478-3482). The error branch before the zero applies only to a pending socket error, which a peer's close does
// not set on a stream socket (uipc_usrreq.c:1081-1117, :2241-2246). A local stream socket reports no source address
// (bsd/kern/uipc_proto.c:86, uipc_syscalls.c:1991-2004), so the wrapper decodes none.
func peekReceive(fd int) (int, error) {
	var b [1]byte
	n, _, err := unix.Recvfrom(fd, b[:], unix.MSG_PEEK|unix.MSG_DONTWAIT)
	if err != nil {
		return n, os.NewSyscallError("recvfrom", err)
	}
	return n, nil
}
