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

package venue

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// query runs a system database reader without inheriting HOME, loader options,
// or name-resolution settings. A warning can indicate an incomplete result.
func query(path string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.WaitDelay = time.Second
	cmd.Env = []string{"LC_ALL=C", "PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	if stderr.Len() != 0 {
		return "", errors.New("system query reported a warning")
	}
	return string(out), nil
}

// homeDir queries the password database by effective UID. os/user may fall
// back to environment variables when built without cgo and a record is absent.
func homeDir() (string, error) {
	uid := strconv.Itoa(os.Geteuid())
	var out string
	var err error
	switch runtime.GOOS {
	case "linux":
		out, err = query("/usr/bin/getent", "passwd", uid)
	case "darwin":
		out, err = query("/usr/bin/dscacheutil", "-q", "user", "-a", "uid", uid)
	default:
		return "", errors.New("no password database probe on this platform")
	}
	if err != nil {
		return "", err
	}
	return accountHome(runtime.GOOS, out, uid)
}

func accountHome(platform, out, uid string) (string, error) {
	record, ok := strings.CutSuffix(out, "\n")
	if !ok || strings.ContainsAny(record, "\r\x00") {
		return "", errors.New("invalid account record")
	}
	switch platform {
	case "linux":
		fields := strings.Split(record, ":")
		if !strings.Contains(record, "\n") && len(fields) == 7 && fields[2] == uid && fields[5] != "" {
			return fields[5], nil
		}
	case "darwin":
		// dscacheutil separates records with a blank line. A blank inside a
		// record, a continuation, or a repeated key makes the reply ambiguous.
		fields := make(map[string]string, 7)
		for line := range strings.SplitSeq(strings.TrimSuffix(record, "\n"), "\n") {
			key, value, ok := strings.Cut(line, ": ")
			if !ok {
				return "", errors.New("malformed account field")
			}
			switch key {
			case "name", "password", "uid", "gid", "dir", "shell", "gecos":
			default:
				return "", errors.New("unknown account field")
			}
			if _, exists := fields[key]; exists {
				return "", errors.New("duplicate account field")
			}
			fields[key] = value
		}
		if len(fields) == 7 && fields["uid"] == uid && fields["dir"] != "" {
			return fields["dir"], nil
		}
	}
	return "", errors.New("no unique password database record")
}

// listenerProbe returns a lazy probe: Linux reads the kernel TCP listener
// table per port. Darwin takes one lsof snapshot for both ports; an unprivileged
// listing protects against a gateway running as this user, not all users.
// Neither probe connects to or sends traffic to a gateway.
func listenerProbe() func(string) (bool, error) {
	if runtime.GOOS == "darwin" {
		snapshot := sync.OnceValues(darwinListeners)
		return func(port string) (bool, error) { busy, err := snapshot(); return busy[port], err }
	}
	return func(port string) (bool, error) {
		if runtime.GOOS != "linux" {
			return false, errors.New("no listener probe on this platform")
		}
		out, err := query("/usr/bin/ss", "-ltn", "sport = :"+port)
		if err != nil {
			return false, err
		}
		return ssListener(out, port)
	}
}

func ssListener(out, port string) (bool, error) {
	table, ok := strings.CutSuffix(out, "\n")
	if !ok {
		return false, errors.New("unterminated TCP table")
	}
	header, rows, hasRows := strings.Cut(table, "\n")
	if strings.Join(strings.Fields(header), " ") != "State Recv-Q Send-Q Local Address:Port Peer Address:Port" {
		return false, errors.New("invalid ss header")
	}
	if !hasRows {
		return false, nil
	}
	for line := range strings.SplitSeq(rows, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 5 || fields[0] != "LISTEN" {
			return false, errors.New("invalid ss listener row")
		}
		for _, q := range fields[1:3] {
			if _, err := strconv.ParseUint(q, 10, 64); err != nil {
				return false, errors.New("invalid TCP queue length")
			}
		}
		if local, ok := endpointPort(fields[3]); !ok || local != port {
			return false, errors.New("unexpected local endpoint in filtered TCP table")
		}
		if _, ok := endpointPort(fields[4]); !ok {
			return false, errors.New("invalid remote TCP endpoint")
		}
	}
	return true, nil
}

// endpointPort accepts numeric addresses and the socket tools' wildcard forms.
func endpointPort(endpoint string) (string, bool) {
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		return "", false
	}
	if host != "*" {
		if _, err := netip.ParseAddr(host); err != nil {
			return "", false
		}
	}
	if port == "*" {
		return port, true
	}
	n, err := strconv.ParseUint(port, 10, 16)
	return strconv.FormatUint(n, 10), err == nil
}
