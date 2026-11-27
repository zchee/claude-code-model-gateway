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
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// listeningState reports whether the socket fd listens. Linux answers through the SO_ACCEPTCONN socket option
// (socket(7)), so the state is always known here.
func listeningState(fd int) (listening, known bool, err error) {
	v, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ACCEPTCONN)
	if err != nil {
		return false, false, os.NewSyscallError("getsockopt", err)
	}
	return v != 0, true, nil
}

// tcpProtocol refuses an IPv4 stream socket whose transport is not TCP. On Linux the family and the stream type
// also describe a one-to-one SCTP socket (sctp(7)), and Go's net.FileListener wraps it as a TCP listener, since it
// classifies by family and type only; the protocol is read through SO_PROTOCOL (socket(7)). Multipath TCP is
// accepted besides plain TCP: since Go 1.24 net.ListenConfig creates an IPPROTO_MPTCP socket for "tcp" and "tcp4"
// whenever the kernel supports it (src/net/dial.go:32 defaultMPTCPEnabledListen, src/net/mptcpsock_linux.go:76-84),
// so the supervisor's own listener is of that protocol on such a kernel, and MPTCP serves a plain TCP client.
func tcpProtocol(fd int) error {
	protocol, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_PROTOCOL)
	if err != nil {
		return os.NewSyscallError("getsockopt", err)
	}
	if protocol != unix.IPPROTO_TCP && protocol != unix.IPPROTO_MPTCP {
		return fmt.Errorf("a stream socket of protocol %d, not TCP", protocol)
	}
	return nil
}
