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

// Package proxyconfig resolves provider settings from the proxy configuration and environment.
package proxyconfig

import "github.com/zchee/claude-code-model-gateway/internal/config"

// InvalidSetting names the source of an invalid setting and the reason for rejecting it.
type InvalidSetting struct {
	Setting string
	Reason  string
}

// StudioSettings describes the enabled, disabled or invalid LM Studio configuration.
type StudioSettings struct {
	Enabled bool
	BaseURL string
	APIKey  string
	Invalid *InvalidSetting
}

// ReadStudio resolves LM Studio settings from the environment and config.json in the given directory.
func ReadStudio(env config.LookupEnv, configDir string) StudioSettings { return StudioSettings{} }
