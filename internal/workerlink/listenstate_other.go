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

//go:build unix && !linux

package workerlink

// listeningState reports the listening state as unknown. macOS does not implement the SO_ACCEPTCONN socket option
// and fails it with ENOPROTOOPT (xnu tests/sockopt_sol_socket.c:879-891), so a bound socket that is not listening is
// imported there and refused by its first Accept instead: XNU's accept fails with EINVAL on a stream socket without
// SO_ACCEPTCONN (bsd/kern/uipc_syscalls.c:502-508), and Go returns that error at once (src/internal/poll/fd_unix.go,
// which retries only EINTR, EAGAIN and ECONNABORTED). The other Unix systems take the same path, which is safe for
// the same reason.
func listeningState(int) (listening, known bool, err error) { return false, false, nil }
