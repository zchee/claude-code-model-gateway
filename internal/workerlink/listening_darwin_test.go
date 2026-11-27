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

// listeningObservable says that macOS does not answer the listening state of a socket: XNU does not implement
// getsockopt SO_ACCEPTCONN and fails it with ENOPROTOOPT (xnu tests/sockopt_sol_socket.c:879-891). The tests there
// identify a listener by its family and bound port instead.
const listeningObservable = false

// otherLoopbackBindable says that macOS cannot bind 127.0.0.2 without configuration: lo0 carries only 127.0.0.1,
// and a bind to an address no interface has fails with EADDRNOTAVAIL (xnu bsd/netinet/in_pcb.c:908-912).
const otherLoopbackBindable = false

// listeningOf asks nothing on macOS and reports the state as not observed.
func listeningOf(int) (listenState, error) { return "", nil }
