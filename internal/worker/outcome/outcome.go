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

// Package outcome carries worker routing and response facts without retaining message content.
package outcome

// Overflow describes a context refusal classified by the response writer.
type Overflow struct {
	// Phase and RefusedBy identify the request phase and the source of the refusal.
	Phase, RefusedBy string
	// TokensFrom identifies the source of the token count; an empty source projects to null.
	TokensFrom string
	// RequestBytes is the size of the request used to diagnose the refusal.
	RequestBytes int64
	// Tokens and LimitTokens are the measured count and ceiling; nil means unknown.
	Tokens, LimitTokens *float64
}

// Compaction contains the guard's terminal assessment across all attempts at compaction.
type Compaction struct {
	// Outcome, TerminalCode and ErrorCode are the guard's bounded diagnostic labels.
	Outcome, TerminalCode, ErrorCode string
	// UpstreamStatus is the HTTP status observed by the guard, if available.
	UpstreamStatus *int
	// InputTokens, OutputTokens, CacheReadTokens and CacheCreationTokens are absent when unreported.
	InputTokens, OutputTokens, CacheReadTokens, CacheCreationTokens *float64
}

// Route combines the worker's selected route with any terminal diagnostics from its response handlers.
type Route struct {
	// SelectedModel and EffectiveModel identify the client selection and resolved model.
	SelectedModel, EffectiveModel string
	// Backend, Effort and Via describe the worker's routing decision.
	Backend, Effort, Via string
	// Fallback reports whether the worker selected a fallback route.
	Fallback bool
	// Compaction is set by the guard when it classifies a compaction attempt sequence.
	Compaction *Compaction
	// Overflow is set by the response writer for a classified context refusal.
	Overflow *Overflow
}

// Audit carries only the route and dispatch metadata prepared by the branch for the local request log.
type Audit struct {
	// Backend identifies the selected provider.
	Backend string
	// Model is the model recorded by the branch, if known.
	Model *string
	// Via and Effort describe how the branch resolved the request and its reasoning effort.
	Via, Effort string
	// SessionID, AgentID, ParentAgentID and InheritedFromAgentID describe the dispatch lineage.
	SessionID, AgentID, ParentAgentID, InheritedFromAgentID string
	// SessionSource identifies how the branch obtained the dispatch session.
	SessionSource string
	// MarkersLength is the marker count when the branch inspected markers, including zero.
	MarkersLength *int
}

// Result reports one terminal outcome to the worker, independently of admission accounting.
type Result struct {
	// Route is the resolved route with the branch's terminal diagnostics attached.
	Route Route
	// Status is the status reported to the span, not necessarily the status written to the client.
	Status int
	// StatusOverride is the branch's terminal label when the status alone is insufficient.
	StatusOverride string
	// Contacted reports whether the branch invoked an inference send, even if dialing failed.
	Contacted bool
	// Cancelled suppresses terminal route publication when client cancellation won.
	Cancelled bool
	// Audit carries a branch's log-only decision when no admission callback published it.
	Audit *Audit
}
