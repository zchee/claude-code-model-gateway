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

import (
	"cmp"
	"os"
	"path/filepath"
	"unicode/utf16"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

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

// ReadStudio resolves LM Studio settings from the environment and config.json in configDir.
// configDir must be a resolved absolute path. The caller resolves a relative directory override
// against its injected working directory.
func ReadStudio(env config.LookupEnv, configDir string) StudioSettings {
	file := readStudioFile(configDir)
	if !*cmp.Or(config.LMStudioEnabled.Get(env), file.enabled, new(true)) {
		return StudioSettings{}
	}

	const defaultOrigin = "http://127.0.0.1:1234"
	setting := "CCP_LMSTUDIO_BASE_URL"
	origin := config.LMStudioBaseURL.Get(env)
	if origin == (config.LMOrigin{}) {
		setting = "lmstudio.baseUrl in config.json"
		origin = config.LMStudioBaseURL.Get(func(string) (string, bool) { return file.baseURL, true })
	}
	if origin.Reason != "" {
		return StudioSettings{Enabled: true, Invalid: &InvalidSetting{Setting: setting, Reason: origin.Reason}}
	}
	key := config.LMStudioAPIKey.Get(env)
	if key == "" {
		key = config.LMToken.Get(env)
	}
	if key == "" {
		key = config.LMStudioAPIKey.Get(func(string) (string, bool) { return file.apiKey, true })
	}
	return StudioSettings{Enabled: true, BaseURL: cmp.Or(origin.Origin, defaultOrigin), APIKey: key}
}

type studioFile struct {
	enabled *bool
	baseURL string
	apiKey  string
}

func readStudioFile(dir string) studioFile {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return studioFile{}
	}
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return studioFile{}
	}
	root, err := jsonx.DecodeWire(data)
	if err != nil {
		return studioFile{}
	}
	for _, member := range root.Members {
		if string(utf16.Decode(member.Name)) == "lmstudio" {
			return parseStudioSection(member.Value)
		}
	}
	return studioFile{}
}

func parseStudioSection(section jsonx.WireValue) studioFile {
	var file studioFile
	for _, field := range section.Members {
		value := field.Value
		if value.Kind == 'n' {
			continue
		}
		switch string(utf16.Decode(field.Name)) {
		case "enabled":
			if value.Kind != 't' && value.Kind != 'f' {
				return studioFile{}
			}
			file.enabled = new(value.Kind == 't')
		case "baseUrl":
			if value.Kind != '"' {
				return studioFile{}
			}
			file.baseURL = string(utf16.Decode(value.Text))
		case "apiKey":
			if value.Kind != '"' {
				return studioFile{}
			}
			file.apiKey = string(utf16.Decode(value.Text))
		}
	}
	return file
}
