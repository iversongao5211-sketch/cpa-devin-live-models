// Package core contains the host-agnostic logic of the cpa-devin-live-models
// plugin. It never imports cgo; the package-main shim in the repository root
// adapts these types to the cliproxy C ABI.
//
// Wire notes (verified against internal/pluginhost + sdk/pluginapi):
//   - pluginapi structs have no json tags: the wire uses exact Go field names
//     ("ID", "DisplayName", "Thinking", ...). Host->plugin helper types do use
//     snake_case json tags (auth_index, files, json...). encoding/json matches
//     case-insensitively on decode but never strips underscores, so we emit the
//     exact expected spellings on every path.
//   - []byte fields travel as base64 JSON strings in both directions.
//   - Envelope shape: {"ok":bool,"result":<json>,"error":{code,message}}.
package core

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// ---------------- lifecycle (plugin.register / plugin.reconfigure) ----------------

// lifecycleRequest mirrors rpcLifecycleRequest (host side) - json tags are
// snake_case on this one.
type lifecycleRequest struct {
	ConfigYAML    string `json:"config_yaml"` // base64-encoded YAML document
	SchemaVersion uint32 `json:"schema_version"`
}

func decodeLifecycle(raw json.RawMessage) (lifecycleRequest, []byte, error) {
	var req lifecycleRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return req, nil, fmt.Errorf("decode lifecycle request: %w", err)
	}
	var cfg []byte
	if s := strings.TrimSpace(req.ConfigYAML); s != "" {
		decoded, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			// Some hosts may pass raw YAML instead of base64; accept both.
			cfg = []byte(s)
		} else {
			cfg = decoded
		}
	}
	return req, cfg, nil
}

// pluginMetadata mirrors pluginapi.Metadata (wire = Go field names).
type pluginMetadata struct {
	Name             string
	Version          string
	Author           string
	GitHubRepository string
	Logo             string
	Description      string `json:",omitempty"`
	ConfigFields     []configField
}

// configField mirrors pluginapi.ConfigField (wire = Go field names).
type configField struct {
	Name        string
	Type        string
	Label       string
	Description string
	Default     any
	EnumValues  []string `json:",omitempty"`
}

// registrationResult is emitted as the result of plugin.register/reconfigure.
type registrationResult struct {
	SchemaVersion uint32           `json:"schema_version"`
	Metadata      pluginMetadata   `json:"metadata"`
	Capabilities  capabilityResult `json:"capabilities"`
}

// capabilityResult mirrors rpcCapabilities (snake_case tags on the host side).
type capabilityResult struct {
	ModelRegistrar bool `json:"model_registrar,omitempty"`
	ModelProvider  bool `json:"model_provider,omitempty"`
	ManagementAPI  bool `json:"management_api,omitempty"`
}

// ---------------- model provider (model.static / model.for_auth) ----------------

// thinkingSupport mirrors pluginapi.ThinkingSupport (wire = Go field names).
type thinkingSupport struct {
	Levels         []string
	Min            int  `json:",omitempty"`
	Max            int  `json:",omitempty"`
	ZeroAllowed    bool `json:",omitempty"`
	DynamicAllowed bool `json:",omitempty"`
}

// modelInfo mirrors pluginapi.ModelInfo (wire = Go field names).
type modelInfo struct {
	ID                         string
	Object                     string
	Created                    int64
	OwnedBy                    string
	Type                       string
	DisplayName                string
	Name                       string
	Version                    string
	Description                string
	InputTokenLimit            int64
	OutputTokenLimit           int64
	SupportedGenerationMethods []string
	ContextLength              int64
	MaxCompletionTokens        int64
	SupportedParameters        []string
	SupportedInputModalities   []string
	SupportedOutputModalities  []string
	Thinking                   *thinkingSupport
	UserDefined                bool
}

// hostConfigSummary mirrors pluginapi.HostConfigSummary (wire = Go field names).
type hostConfigSummary struct {
	AuthDir          string
	ProxyURL         string
	ForceModelPrefix bool
	OAuthModelAlias  map[string][]modelAlias
	ExcludedModels   map[string][]string
}

type modelAlias struct {
	Name        string
	Alias       string
	DisplayName string
	Fork        bool
}

// authModelRequest mirrors pluginapi.AuthModelRequest + rpcAuthModelRequest.
type authModelRequest struct {
	Plugin         pluginMetadata
	AuthID         string
	AuthProvider   string
	StorageJSON    json.RawMessage // []byte on the wire (base64 decoded by encoding/json automatically)
	Metadata       map[string]any
	Attributes     map[string]string
	Host           hostConfigSummary
	HostCallbackID string `json:"host_callback_id"`
}

// staticModelRequest mirrors pluginapi.StaticModelRequest.
type staticModelRequest struct {
	Plugin         pluginMetadata
	Host           hostConfigSummary
	HostCallbackID string `json:"host_callback_id"`
}

// modelResponse mirrors pluginapi.ModelResponse (wire = Go field names).
type modelResponse struct {
	Provider string      `json:"Provider"`
	Models   []modelInfo `json:"Models"`
}

// ---------------- host calls (plugin -> host) ----------------

// hostAuthFileEntry mirrors pluginapi.HostAuthFileEntry (snake_case tags).
type hostAuthFileEntry struct {
	ID          string `json:"id,omitempty"`
	AuthIndex   string `json:"auth_index,omitempty"`
	Name        string `json:"name"`
	Type        string `json:"type,omitempty"`
	Provider    string `json:"provider,omitempty"`
	Label       string `json:"label,omitempty"`
	Status      string `json:"status,omitempty"`
	Disabled    bool   `json:"disabled,omitempty"`
	Unavailable bool   `json:"unavailable,omitempty"`
	RuntimeOnly bool   `json:"runtime_only,omitempty"`
	Source      string `json:"source,omitempty"`
	Path        string `json:"path,omitempty"`
}

type hostAuthListResponse struct {
	Files []hostAuthFileEntry `json:"files"`
}

type hostAuthGetResponse struct {
	AuthIndex string          `json:"auth_index"`
	Name      string          `json:"name,omitempty"`
	Path      string          `json:"path,omitempty"`
	JSON      json.RawMessage `json:"json"`
}

// ---------------- management API ----------------

type managementRoute struct {
	Method      string
	Path        string
	Menu        string `json:",omitempty"`
	Description string `json:",omitempty"`
}

// managementRegistration mirrors rpcManagementRegistrationResponse.
type managementRegistration struct {
	Routes []managementRoute `json:"routes,omitempty"`
}

// managementRequest mirrors pluginapi.ManagementRequest + rpcManagementRequest.
type managementRequest struct {
	Method         string
	Path           string
	Headers        map[string][]string
	Query          map[string][]string
	Body           []byte
	HostCallbackID string `json:"host_callback_id"`
}

// managementResponse mirrors pluginapi.ManagementResponse; Body is []byte so it
// round-trips as base64 on the wire.
type managementResponse struct {
	StatusCode int
	Headers    map[string][]string
	Body       []byte
}

func jsonManagementResponse(status int, v any) managementResponse {
	raw, err := json.Marshal(v)
	if err != nil {
		status = 500
		raw = []byte(`{"ok":false,"error":"marshal failed"}`)
	}
	return managementResponse{
		StatusCode: status,
		Headers:    map[string][]string{"Content-Type": {"application/json; charset=utf-8"}},
		Body:       raw,
	}
}
