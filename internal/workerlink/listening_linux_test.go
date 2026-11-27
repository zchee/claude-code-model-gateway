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

package workerlink_test

import "golang.org/x/sys/unix"

// listeningObservable says that Linux answers the listening state of a socket: getsockopt SO_ACCEPTCONN, socket(7).
const listeningObservable = true

// otherLoopbackBindable says that a socket can be bound to 127.0.0.2 without configuration: Linux treats the whole
// of 127.0.0.0/8 as local through the route the kernel installs with the loopback address (ip-route(8), table local).
const otherLoopbackBindable = true

// listeningOf reports whether the socket fd listens.
func listeningOf(fd int) (listenState, error) {
	v, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ACCEPTCONN)
	if err != nil {
		return "", err
	}
	if v != 0 {
		return isListening, nil
	}
	return notListening, nil
}
