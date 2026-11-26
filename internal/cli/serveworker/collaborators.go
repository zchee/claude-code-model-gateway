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

package serveworker

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/paths"
	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/telemetry"
	"github.com/zchee/claude-code-model-gateway/internal/upstreamstate"
	"github.com/zchee/claude-code-model-gateway/internal/usage"
	"github.com/zchee/claude-code-model-gateway/internal/wiring"
	"github.com/zchee/claude-code-model-gateway/internal/worker"
	"github.com/zchee/claude-code-model-gateway/internal/worker/compactguard"
	"github.com/zchee/claude-code-model-gateway/internal/worker/compose"
	"github.com/zchee/claude-code-model-gateway/internal/worker/modelcache"
	"github.com/zchee/claude-code-model-gateway/internal/worker/proxyreq"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// syncWriter serializes whole Write calls of the worker's request handlers and background work onto
// one output stream.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// lifetime owns the background work of one worker: the catalog refreshes it starts and the exports,
// connections and timers its requests leave behind.
type lifetime struct {
	ctx       context.Context
	cancel    context.CancelFunc
	cache     *modelcache.Cache
	upstream  *upstream.Client
	auth      *http.Transport
	usage     *usage.Emitter
	telemetry *telemetry.Recorder
}

// start begins the catalog refreshes under the worker context.
func (l *lifetime) start() { l.cache.Start(l.ctx) }

// stop ends the worker context and joins everything that ran under it. Requests have stopped by then.
func (l *lifetime) stop() {
	l.cancel()
	l.cache.Wait()
	_ = l.upstream.Close()
	l.auth.CloseIdleConnections()
	l.usage.Wait()
	l.telemetry.Wait()
}

// workerSetup carries what the collaborators are built from.
type workerSetup struct {
	ctx            context.Context
	lookup         config.LookupEnv
	resolver       paths.Resolver
	state          string
	cwd            func() (string, error)
	version, token string
	stdout, stderr io.Writer
	beginDrain     func()
}

// buildWorker constructs the worker's collaborators and composes its handler options. It starts no
// background work; the returned lifetime's cache is started by the caller once it serves.
//
// The context window settings are read here and an unusable file stops the worker, as the source
// fails to load (lib/runtime.js:307-320).
func buildWorker(s workerSetup) (worker.Options, *lifetime, error) {
	windows, err := policy.ReadWindows(filepath.Join(s.state, "context-window.json"), s.lookup)
	if err != nil {
		return worker.Options{}, nil, fmt.Errorf("model-gateway: the context window settings are unusable: %w", err)
	}
	now := time.Now
	startup := s.state
	if s.cwd != nil {
		if dir, err := s.cwd(); err == nil && filepath.IsAbs(dir) {
			startup = dir
		}
	}
	proxyBase := "http://127.0.0.1:" + jsNumber(config.ProxyPort.Get(s.lookup))
	anthropicBase := config.Upstream.Get(s.lookup)
	sentryEnabled := config.Sentry.Get(s.lookup)

	serviceCtx, cancel := context.WithCancel(s.ctx)
	client := upstream.NewClient(upstream.Options{})
	// Credential discovery and refresh never share the inference client's pool or lifetime.
	authTransport := upstream.NewTransport(upstream.Options{})
	life := &lifetime{ctx: serviceCtx, cancel: cancel, upstream: client, auth: authTransport}
	fail := func(err error) (worker.Options, *lifetime, error) {
		cancel()
		_ = client.Close()
		authTransport.CloseIdleConnections()
		return worker.Options{}, nil, err
	}

	routes, state := &route.Store{}, &sentry.State{}
	grokHome, grokErr := s.resolver.GrokHome()
	var grokIDs func() []string
	if grokErr == nil {
		grokIDs = compose.NewGrokModelIDs(filepath.Join(grokHome, "models_cache.json"))
	}
	// The effective base URL depends on the project the worker was started in; without one the worker
	// cannot be shown to be wired, and the discovery cache is left alone.
	var effectiveBaseURL func() (string, error)
	if s.cwd != nil {
		if dir, err := s.cwd(); err == nil && filepath.IsAbs(dir) {
			settings := wiring.Store{Paths: s.resolver, Project: dir, Lookup: s.lookup}
			effectiveBaseURL = func() (string, error) {
				r, err := settings.EffectiveBaseURL()
				if err != nil || r.Winner == nil {
					return "", err
				}
				return r.Winner.Value, nil
			}
		}
	}
	cache, err := modelcache.New(modelcache.Options{
		Lookup:           s.lookup,
		Paths:            s.resolver,
		Windows:          windows,
		ProxyBaseURL:     proxyBase,
		AnthropicBaseURL: anthropicBase,
		DefaultBaseURL:   "http://127.0.0.1:" + jsNumber(config.PublicPort.Get(s.lookup)),
		Client:           &http.Client{},
		Clock:            now,
		Routes:           routes,
		Sentry:           state,
		EffectiveBaseURL: effectiveBaseURL,
		GrokIDs:          grokIDs,
		Logf: func(format string, args ...any) {
			_, _ = fmt.Fprintf(s.stdout, format+"\n", args...)
		},
	})
	if err != nil {
		return fail(err)
	}
	life.cache = cache

	var grokAuth, grokVersion string
	if grokErr == nil {
		grokAuth, grokVersion = filepath.Join(grokHome, "auth.json"), filepath.Join(grokHome, "version.json")
	}
	grokBranch, err := compose.NewGrokBranch(compose.GrokOptions{
		Endpoint:        config.GrokEndpoint.Get(s.lookup),
		VersionFile:     grokVersion,
		AuthFile:        grokAuth,
		InferenceClient: client,
		AuthTransport:   authTransport,
		Sentry:          state,
		SentryEnabled:   sentryEnabled,
		Now:             now,
	})
	if err != nil {
		return fail(err)
	}
	dispatchPath, err := s.resolver.DispatchCachePath()
	if err != nil {
		return fail(fmt.Errorf("model-gateway: could not resolve the dispatch route cache: %w", err))
	}
	prepare, err := proxyreq.New(proxyreq.Options{StartupDir: startup, DispatchPath: dispatchPath, Lookup: s.lookup, Now: now, Windows: windows})
	if err != nil {
		return fail(err)
	}

	projects, _ := s.resolver.TranscriptsDir()
	life.usage = usage.NewEmitter(s.lookup, usage.NewBuilder(usage.Options{ProjectsDir: projects, Now: now}))
	var highWater *usage.HighWater
	if dir, err := s.resolver.RequestBodyDir(); err == nil {
		highWater = usage.NewHighWater(dir, now)
	}
	var logPath string
	if config.RequestLog.Get(s.lookup) {
		logPath, _ = s.resolver.RequestLogPath()
	}
	life.telemetry = telemetry.NewRecorder(telemetry.Options{Lookup: s.lookup, LogPath: logPath, Diagnostics: s.stderr, Now: now})

	// The running executable stands in for the stable binary in operator advice until one is installed.
	command, _ := os.Executable()
	options, err := compose.New(compose.Options{
		ServiceContext:   serviceCtx,
		Version:          s.version,
		ControlToken:     s.token,
		Lookup:           s.lookup,
		AnthropicBaseURL: anthropicBase,
		ProxyBaseURL:     proxyBase,
		CommandPath:      command,
		InstalledVersion: s.version,
		LogsDir:          filepath.Join(s.state, "logs"),
		Windows:          windows,
		SentryEnabled:    sentryEnabled,
		SentryEnvTrigger: config.CompactTrigger.Get(s.lookup),
		Routes:           routes,
		ModelCache:       cache,
		Sentry:           state,
		Upstream:         client,
		UpstreamState:    &upstreamstate.Store{Dir: s.state, Now: now, CommandPath: command},
		ProxyReq:         prepare,
		CompactGuard:     &compactguard.Runner{ServiceContext: serviceCtx, Settings: compactguard.LoadSettings(s.lookup), Stderr: s.stderr},
		Grok:             grokBranch,
		Usage:            life.usage,
		HighWater:        highWater,
		Telemetry:        life.telemetry,
		Now:              now,
		BeginDrain:       s.beginDrain,
		Diagnostics:      s.stderr,
	})
	if err != nil {
		return fail(err)
	}
	return options, life, nil
}
