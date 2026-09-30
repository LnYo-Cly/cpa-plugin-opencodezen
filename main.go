// SPDX-License-Identifier: MIT
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	int (*call)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
	void (*free_buffer)(void*, size_t);
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

static cliproxy_host_api* stored_host;

static void store_host_api(cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unsafe"

	"gopkg.in/yaml.v3"
)

const abiVersion uint32 = 1

const (
	pluginID       = "zen"
	pluginProvider = "zen"

	zenBaseURL = "https://opencode.ai/zen/v1"

	maxSessionIDBytes = 1024

	targetSessionHeader = "X-Opencode-Session"
	targetRequestHeader = "X-Opencode-Request"
	targetClientHeader  = "X-Opencode-Client"
	targetProjectHeader = "X-Opencode-Project"

	defaultUserAgent = "opencode/1.18.31 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14"
)

var pluginVersion = "0.7.3"

var (
	canonicalSessionRe = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
)

// sessionSources lists downstream client headers that carry a stable
// per-conversation session id, in priority order (same semantics as
// opencode-session-mapper).
var sessionSources = []string{
	"Session-Id",
	"Session_id",
	"Thread-Id",
	"Thread_id",
	"X-Claude-Code-Session-Id",
	"X-DeepSeek-Harness-Session-Id",
	"X-Session-Affinity",
	"X-Session-Id",
	"X-Client-Request-Id",
}

// ---------------------------------------------------------------------------
// plugin configuration
// ---------------------------------------------------------------------------

type modelRoute struct {
	Model    string
	Endpoint string // "chat" (default) or "responses"
	Alias    string

	// Optional capability metadata announced to the host model registry.
	// Explicit values override the built-in defaults for the model.
	ContextLength       int64    // yaml: context-length
	MaxCompletionTokens int64    // yaml: max-completion-tokens
	InputModalities     []string // yaml: input-modalities
	ReasoningLevels     []string // yaml: reasoning-levels
}

// EndpointPath returns the upstream path suffix for this route.
func (m modelRoute) EndpointPath() string {
	if m.Endpoint == "responses" {
		return "/responses"
	}
	return "/chat/completions"
}

type pluginConfig struct {
	Enabled       bool
	Provider      string
	BaseURL       string
	APIKeys       []string
	ExcludeModels []string
	Client        string
	Project       string
	Models        []modelRoute
}

func defaultPluginConfig() pluginConfig {
	return pluginConfig{
		Enabled:  true,
		Provider: pluginProvider,
		BaseURL:  zenBaseURL,
		Client:   "cli",
		Project:  "global",
	}
}

var (
	configMu sync.RWMutex
	current  = defaultPluginConfig()
)

func loadedConfig() pluginConfig {
	configMu.RLock()
	defer configMu.RUnlock()
	return current
}

func storeConfig(cfg pluginConfig) {
	configMu.Lock()
	defer configMu.Unlock()
	current = cfg
}

func configure(raw []byte) error {
	cfg := defaultPluginConfig()
	if len(raw) > 0 {
		var req struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			return err
		}
		if len(req.ConfigYAML) > 0 {
			var node map[string]any
			if err := yaml.Unmarshal(req.ConfigYAML, &node); err != nil {
				return err
			}
			applyConfigNode(&cfg, node)
		}
	}
	cfg.Provider = strings.ToLower(strings.TrimSpace(cfg.Provider))
	if cfg.Provider == "" {
		cfg.Provider = pluginProvider
	}
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if cfg.BaseURL == "" {
		cfg.BaseURL = zenBaseURL
	}
	if cfg.Client = strings.TrimSpace(cfg.Client); cfg.Client == "" {
		cfg.Client = "cli"
	}
	if cfg.Project = strings.TrimSpace(cfg.Project); cfg.Project == "" {
		cfg.Project = "global"
	}
	cfg.APIKeys = trimNonEmpty(cfg.APIKeys)
	cfg.ExcludeModels = trimNonEmpty(cfg.ExcludeModels)
	for i := range cfg.Models {
		m := &cfg.Models[i]
		m.Model = strings.TrimSpace(m.Model)
		m.Alias = strings.TrimSpace(m.Alias)
		m.Endpoint = strings.ToLower(strings.TrimSpace(m.Endpoint))
		if m.Endpoint != "responses" {
			m.Endpoint = "chat"
		}
		if m.Alias == "" {
			m.Alias = m.Model
		}
	}
	storeConfig(cfg)
	persistVirtualAuths(cfg)
	return nil
}

// persistVirtualAuths materializes plugin-config api-keys as real credential
// files in the host auth directory via the host.auth.save callback. CPA v8
// only creates runtime auths from files discovered in the auth dir (the
// auth.parse payload carries RawJSON, not StorageJSON), so virtual keys
// configured through the management center must be written to disk to
// become selectable credentials.
func persistVirtualAuths(cfg pluginConfig) {
	if !cfg.Enabled || len(cfg.APIKeys) == 0 {
		return
	}
	for _, key := range cfg.APIKeys {
		id := sha256Prefix(key)
		name := fmt.Sprintf("%s-%s.json", cfg.Provider, id)
		payload, _ := json.Marshal(map[string]string{
			"type":     cfg.Provider,
			"provider": cfg.Provider,
			"api_key":  key,
			"base_url": cfg.BaseURL,
		})
		// Best effort: failures are logged by the host; the auth.parse
		// fallback still returns virtual auths for hosts that support it.
		_, _ = callHost("host.auth.save", map[string]any{
			"Name": name,
			"JSON": json.RawMessage(payload),
		})
	}
}

func trimNonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func applyConfigNode(cfg *pluginConfig, node map[string]any) {
	if node == nil {
		return
	}
	if v, ok := node["enabled"]; ok {
		cfg.Enabled = boolValue(v)
	}
	if v, ok := node["provider"].(string); ok {
		cfg.Provider = v
	}
	if v, ok := node["base-url"].(string); ok {
		cfg.BaseURL = v
	}
	if v, ok := node["client"].(string); ok {
		cfg.Client = v
	}
	if v, ok := node["project"].(string); ok {
		cfg.Project = v
	}
	switch t := node["api-keys"].(type) {
	case []any:
		for _, item := range t {
			if s, ok := item.(string); ok {
				cfg.APIKeys = append(cfg.APIKeys, s)
			}
		}
	case string:
		// Accept comma, semicolon, or whitespace separated keys; management
		// center users have used ";" and gotten one invalid concatenated key.
		for _, part := range strings.FieldsFunc(t, func(r rune) bool {
			return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
		}) {
			cfg.APIKeys = append(cfg.APIKeys, part)
		}
	}
	switch t := node["exclude-models"].(type) {
	case []any:
		for _, item := range t {
			if s, ok := item.(string); ok {
				cfg.ExcludeModels = append(cfg.ExcludeModels, s)
			}
		}
	case string:
		for _, part := range strings.FieldsFunc(t, func(r rune) bool {
			return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
		}) {
			cfg.ExcludeModels = append(cfg.ExcludeModels, part)
		}
	}
	if rawModels, ok := node["models"].([]any); ok {
		for _, item := range rawModels {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			m := modelRoute{}
			if s, ok := entry["model"].(string); ok {
				m.Model = s
			}
			if s, ok := entry["name"].(string); ok && m.Model == "" {
				m.Model = s
			}
			if s, ok := entry["endpoint"].(string); ok {
				m.Endpoint = s
			}
			if s, ok := entry["alias"].(string); ok {
				m.Alias = s
			}
			m.ContextLength = intValue(entry["context-length"], entry["context_length"])
			m.MaxCompletionTokens = intValue(entry["max-completion-tokens"], entry["max_completion_tokens"])
			m.InputModalities = stringSliceValue(entry["input-modalities"], entry["input_modalities"])
			m.ReasoningLevels = stringSliceValue(entry["reasoning-levels"], entry["reasoning_levels"])
			if m.Model != "" {
				cfg.Models = append(cfg.Models, m)
			}
		}
	}
}

func boolValue(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return strings.EqualFold(strings.TrimSpace(t), "true")
	default:
		return false
	}
}

// intValue returns the first numeric value among candidates as int64.
func intValue(values ...any) int64 {
	for _, v := range values {
		switch t := v.(type) {
		case int:
			return int64(t)
		case int64:
			return t
		case float64:
			return int64(t)
		}
	}
	return 0
}

// stringSliceValue returns the first non-empty string slice among candidates.
// Accepts a YAML list or a comma-separated string.
func stringSliceValue(values ...any) []string {
	for _, v := range values {
		switch t := v.(type) {
		case []any:
			out := make([]string, 0, len(t))
			for _, item := range t {
				if s, ok := item.(string); ok {
					if s = strings.TrimSpace(s); s != "" {
						out = append(out, s)
					}
				}
			}
			if len(out) > 0 {
				return out
			}
		case string:
			var out []string
			for _, part := range strings.Split(t, ",") {
				if part = strings.TrimSpace(part); part != "" {
					out = append(out, part)
				}
			}
			if len(out) > 0 {
				return out
			}
		}
	}
	return nil
}

// inferEndpoint resolves the upstream endpoint ("chat" or "responses") for a model.
// Any model containing "muse" or "responses" defaults to "responses"; others to "chat".
func inferEndpoint(model string) string {
	lower := strings.ToLower(model)
	if strings.Contains(lower, "muse") || strings.Contains(lower, "responses") {
		return "responses"
	}
	return "chat"
}

// learnedEndpoints records upstream endpoints discovered by retrying a
// failed chat request against /responses (models zen only serves there).
var learnedEndpoints struct {
	mu sync.RWMutex
	m  map[string]string
}

func rememberEndpoint(model, endpoint string) {
	learnedEndpoints.mu.Lock()
	defer learnedEndpoints.mu.Unlock()
	if learnedEndpoints.m == nil {
		learnedEndpoints.m = map[string]string{}
	}
	learnedEndpoints.m[strings.ToLower(strings.TrimSpace(model))] = endpoint
}

func learnedEndpoint(model string) (string, bool) {
	learnedEndpoints.mu.RLock()
	defer learnedEndpoints.mu.RUnlock()
	ep, ok := learnedEndpoints.m[strings.ToLower(strings.TrimSpace(model))]
	return ep, ok
}

// routeConfigured reports whether the model has an explicit entry in plugin
// config; learned-endpoint retries never override explicit configuration.
func routeConfigured(cfg pluginConfig, model string) bool {
	for _, m := range cfg.Models {
		if strings.EqualFold(m.Model, model) || strings.EqualFold(m.Alias, model) {
			return true
		}
	}
	return false
}

// routeForModel resolves the route for a requested model id.
// It checks explicit plugin configuration first; if none match, it automatically
// infers the route so any model added in openai-compatibility works out of the box.
func routeForModel(cfg pluginConfig, model string) (modelRoute, bool) {
	model = strings.TrimSpace(model)
	if model == "" {
		return modelRoute{}, false
	}
	for _, m := range cfg.Models {
		if strings.EqualFold(m.Model, model) || strings.EqualFold(m.Alias, model) {
			return m, true
		}
	}
	// Fallback to intelligent inference, refined by endpoints learned from
	// live 400-retries (chat → responses).
	r := modelRoute{
		Model:    model,
		Alias:    model,
		Endpoint: inferEndpoint(model),
	}
	if ep, ok := learnedEndpoint(model); ok {
		r.Endpoint = ep
	}
	return r, true
}

// ---------------------------------------------------------------------------
// RPC envelope types
// ---------------------------------------------------------------------------

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type registration struct {
	SchemaVersion uint32       `json:"schema_version"`
	Metadata      metadata     `json:"metadata"`
	Capabilities  capabilities `json:"capabilities"`
}

type metadata struct {
	Name             string `json:"Name"`
	Version          string `json:"Version"`
	Author           string `json:"Author"`
	GitHubRepository string `json:"GitHubRepository"`
	Logo             string `json:"Logo"`
	ConfigFields     []any  `json:"ConfigFields"`
}

type capabilities struct {
	Executor              bool     `json:"executor"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats"`
	ExecutorOutputFormats []string `json:"executor_output_formats"`
	ModelRegistrar        bool     `json:"model_registrar"`
	AuthProvider          bool     `json:"auth_provider"`
}

type modelInfo struct {
	ID          string `json:"ID"`
	Object      string `json:"Object"`
	Created     int64  `json:"Created"`
	OwnedBy     string `json:"OwnedBy"`
	Type        string `json:"Type"`
	DisplayName string `json:"DisplayName"`

	// Capability metadata consumed by the host model registry and forwarded
	// to clients as context_window / max_tokens / input_modalities /
	// supported_reasoning_levels. Zero values are omitted.
	ContextLength            int64            `json:"ContextLength,omitempty"`
	MaxCompletionTokens      int64            `json:"MaxCompletionTokens,omitempty"`
	SupportedInputModalities []string         `json:"SupportedInputModalities,omitempty"`
	Thinking                 *thinkingSupport `json:"Thinking,omitempty"`
}

// thinkingSupport mirrors the host plugin SDK ThinkingSupport shape.
type thinkingSupport struct {
	Min            int      `json:"Min,omitempty"`
	Max            int      `json:"Max,omitempty"`
	ZeroAllowed    bool     `json:"ZeroAllowed,omitempty"`
	DynamicAllowed bool     `json:"DynamicAllowed,omitempty"`
	Levels         []string `json:"Levels,omitempty"`
}

// modelCapabilities is the resolved capability metadata for one model.
type modelCapabilities struct {
	ContextLength       int64
	MaxCompletionTokens int64
	InputModalities     []string
	Thinking            *thinkingSupport
}

type modelRegistrationResponse struct {
	Provider string      `json:"Provider"`
	Models   []modelInfo `json:"Models"`
}

type identifierResponse struct {
	Identifier string `json:"identifier"`
}

type executorRequest struct {
	AuthID          string              `json:"AuthID"`
	AuthProvider    string              `json:"AuthProvider"`
	Model           string              `json:"Model"`
	Format          string              `json:"Format"`
	Stream          bool                `json:"Stream"`
	Alt             string              `json:"Alt"`
	Headers         map[string][]string `json:"Headers"`
	OriginalRequest []byte              `json:"OriginalRequest"`
	SourceFormat    string              `json:"SourceFormat"`
	Payload         []byte              `json:"Payload"`
	StorageJSON     []byte              `json:"StorageJSON"`
	AuthMetadata    map[string]any      `json:"AuthMetadata"`
	AuthAttributes  map[string]any      `json:"AuthAttributes"`
	Metadata        map[string]any      `json:"Metadata"`
	StreamID        string              `json:"stream_id,omitempty"`
	HostCallbackID  string              `json:"host_callback_id,omitempty"`
}

type executorResponse struct {
	Payload []byte              `json:"Payload"`
	Headers map[string][]string `json:"Headers,omitempty"`
}

type executorStreamResponse struct {
	Headers map[string][]string `json:"headers,omitempty"`
}

type httpStreamChunk struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

// ---------------------------------------------------------------------------
// exported C ABI
// ---------------------------------------------------------------------------

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(abiVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) (status C.int) {
	status = 1
	if response == nil {
		return 1
	}
	response.ptr = nil
	response.len = 0
	defer func() {
		if recover() != nil {
			writeResponse(response, errorEnvelope("plugin_panic", "plugin call failed"))
			status = 1
		}
	}()
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var payload []byte
	if requestLen > 0 {
		if request == nil {
			writeResponse(response, errorEnvelope("invalid_request", "request buffer is required"))
			return 1
		}
		payload = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, callStatus := processPluginCall(C.GoString(method), payload)
	writeResponse(response, raw)
	return C.int(callStatus)
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, length C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = length
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

// ---------------------------------------------------------------------------
// method dispatch
// ---------------------------------------------------------------------------

func processPluginCall(method string, payload []byte) ([]byte, int) {
	raw, errHandle := handleMethod(method, payload)
	if errHandle != nil {
		return errorEnvelope("plugin_error", errHandle.Error()), 1
	}
	return raw, 0
}

func handleMethod(method string, payload []byte) ([]byte, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		if err := configure(payload); err != nil {
			return nil, err
		}
		return okEnvelopeJSON(registration{
			SchemaVersion: abiVersion,
			Metadata: metadata{
				Name:             pluginID,
				Version:          pluginVersion,
				Author:           "Victor9578",
				GitHubRepository: "https://github.com/Victor9578/cpa-plugin-opencodezen",
				Logo:             "https://raw.githubusercontent.com/Victor9578/cpa-plugin-opencodezen/main/logo.svg",
				ConfigFields: []any{
					map[string]any{"Name": "enabled", "Type": "boolean", "Description": "Enable the zen provider plugin."},
					map[string]any{"Name": "api-keys", "Type": "string", "Description": "OpenCode zen API keys (comma-separated). Each key becomes a virtual zen credential; alternatively drop key files into the auth directory."},
					map[string]any{"Name": "exclude-models", "Type": "string", "Description": "Model IDs (comma-separated) to exclude from the models auto-discovered from the zen /models endpoint."},
				},
			},
			Capabilities: capabilities{
				Executor:              true,
				ExecutorModelScope:    "both",
				ExecutorInputFormats:  []string{"chat-completions", "responses"},
				ExecutorOutputFormats: []string{"chat-completions"},
				ModelRegistrar:        true,
				AuthProvider:          true,
			},
		})
	case "executor.identifier", "auth.identifier":
		return okEnvelopeJSON(identifierResponse{Identifier: loadedConfig().Provider})
	case "auth.parse":
		return authParse(payload)
	case "model.register":
		return modelRegistration()
	case "executor.execute":
		return execute(payload, false)
	case "executor.execute_stream":
		return execute(payload, true)
	case "executor.count_tokens":
		return okEnvelopeJSON(executorResponse{Payload: []byte(`{"total_tokens":0}`)})
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// ---------------------------------------------------------------------------
// auth provider
// ---------------------------------------------------------------------------

// authParse inspects credential files discovered in the auth directory.
// It recognizes:
//  1. Files where provider is "zen" or type is "zen"
//  2. Any file containing an "api_key" or "key" field with "sk-" prefix
//  3. Files matching configured keys in plugins.configs.zen
//
// The host sends the auth file payload as RawJSON (AuthParseRequest.RawJSON
// in CPA v8); StorageJSON is accepted as well for compatibility with hosts
// that deliver it under that name.
func authParse(payload []byte) ([]byte, error) {
	cfg := loadedConfig()
	if len(payload) > 0 {
		var req struct {
			Provider    string          `json:"Provider"`
			StorageJSON json.RawMessage `json:"StorageJSON"`
			RawJSON     json.RawMessage `json:"RawJSON"`
			FileName    string          `json:"FileName"`
			ID          string          `json:"ID"`
		}
		if err := json.Unmarshal(payload, &req); err == nil {
			raw := req.StorageJSON
			if len(raw) == 0 {
				raw = req.RawJSON
			}
			var stored map[string]any
			if len(raw) > 0 {
				_ = json.Unmarshal(raw, &stored)
			}
			if len(stored) > 0 {
				key := extractAPIKey(stored)
				pName := extractProvider(stored, req.Provider)

				// If the file explicitly mentions zen, or if the host identifies it as zen,
				// or if it matches any configured api-key. Filename/ID only need the
				// provider prefix so zen.json, zen1.json, zen-abc.json all match.
				isZen := strings.EqualFold(pName, cfg.Provider) ||
					strings.HasPrefix(strings.ToLower(req.FileName), cfg.Provider) ||
					strings.HasPrefix(strings.ToLower(req.ID), cfg.Provider)

				if !isZen && key != "" {
					for _, k := range cfg.APIKeys {
						if k == key {
							isZen = true
							break
						}
					}
				}

				if isZen && key != "" {
					// Remember the key so model discovery can query /models, and
					// refresh the discovered free-tier list in the background.
					rememberAuthKey(key)
					go refreshDiscoveredModels(authBaseURL(stored, cfg), key)
					id := sha256Prefix(key)
					fileName := req.FileName
					if fileName == "" {
						fileName = fmt.Sprintf("%s-%s.json", cfg.Provider, id)
					}
					authID := req.ID
					if authID == "" {
						authID = fmt.Sprintf("%s-%s", cfg.Provider, id)
					}
					modelsList := extractAuthModels(stored)
					return okEnvelopeJSON(authParseResponse{
						Handled: true,
						Auth: authData{
							Provider:    cfg.Provider,
							ID:          authID,
							FileName:    fileName,
							Label:       authLabel(cfg.Provider, id),
							StorageJSON: raw,
							Models:      modelsList,
							Metadata:    map[string]any{"type": cfg.Provider},
						},
					})
				}
			}
		}
	}

	// Fallback for virtual auths from plugin config (if any configured)
	if len(cfg.APIKeys) == 0 {
		return okEnvelopeJSON(authParseResponse{Handled: false})
	}
	auths := make([]authData, 0, len(cfg.APIKeys))
	for _, key := range cfg.APIKeys {
		if key == "" {
			continue
		}
		storageJSON, _ := json.Marshal(map[string]string{
			"provider": cfg.Provider,
			"type":     cfg.Provider,
			"api_key":  key,
		})
		id := sha256Prefix(key)
		auths = append(auths, authData{
			Provider:    cfg.Provider,
			ID:          fmt.Sprintf("%s-%s", cfg.Provider, id),
			FileName:    fmt.Sprintf("%s-%s.json", cfg.Provider, id),
			Label:       authLabel(cfg.Provider, id),
			StorageJSON: storageJSON,
			Metadata:    map[string]any{"type": cfg.Provider},
		})
	}
	return okEnvelopeJSON(authParseResponse{Handled: true, Auths: auths})
}

func extractAPIKey(stored map[string]any) string {
	for _, field := range []string{"api_key", "api-key", "key", "token", "access_token"} {
		if v, ok := stored[field].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func extractProvider(stored map[string]any, hostProvider string) string {
	if v, ok := stored["provider"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if v, ok := stored["type"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return strings.TrimSpace(hostProvider)
}

func extractAuthModels(stored map[string]any) []any {
	raw, ok := stored["models"]
	if !ok {
		return nil
	}
	switch t := raw.(type) {
	case []any:
		return t
	default:
		return nil
	}
}

func authLabel(provider, id string) string {
	if provider == "zen" {
		return fmt.Sprintf("OpenCode Zen (%s…)", id)
	}
	return fmt.Sprintf("%s (%s…)", provider, id)
}

type authParseResponse struct {
	Handled bool       `json:"Handled"`
	Auth    authData   `json:"Auth,omitempty"`
	Auths   []authData `json:"Auths,omitempty"`
}

type authData struct {
	Provider         string          `json:"Provider"`
	ID               string          `json:"ID"`
	FileName         string          `json:"FileName"`
	Label            string          `json:"Label"`
	Prefix           string          `json:"Prefix"`
	ProxyURL         string          `json:"ProxyURL"`
	Disabled         bool            `json:"Disabled"`
	StorageJSON      json.RawMessage `json:"StorageJSON"`
	Models           []any           `json:"Models,omitempty"`
	Metadata         map[string]any  `json:"Metadata"`
	Attributes       map[string]any  `json:"Attributes"`
	NextRefreshAfter string          `json:"NextRefreshAfter"`
}

func sha256Prefix(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:4])
}

// ---------------------------------------------------------------------------
// model registration
// ---------------------------------------------------------------------------

// defaultZenModels announces the free-tier Zen models supported out of the box.
var defaultZenModels = []modelRoute{
	{Model: "mimo-v2.6-flash-free", Alias: "mimo-v2.6-flash-free", Endpoint: "chat"},
	{Model: "mimo-v2.5-free", Alias: "mimo-v2.5-free", Endpoint: "chat"},
	{Model: "ling-3.0-flash-fin-free", Alias: "ling-3.0-flash-fin-free", Endpoint: "chat"},
	{Model: "nemotron-3-ultra-free", Alias: "nemotron-3-ultra-free", Endpoint: "chat"},
	{Model: "muse-spark-1.3-contributor-free", Alias: "muse-spark-1.3-contributor-free", Endpoint: "responses"},
}

// defaultModelCapabilities announces built-in capability metadata for known
// Zen free-tier models. Values can be overridden per model through
// plugins.configs.zen models entries (context-length, max-completion-tokens,
// input-modalities, reasoning-levels).
var defaultModelCapabilities = map[string]modelCapabilities{
	"mimo-v2.6-flash-free": {ContextLength: 1000000},
	"mimo-v2.5-free":       {ContextLength: 1000000},
}

// resolveModelCapabilities merges built-in defaults with explicit route
// configuration; explicit values win.
func resolveModelCapabilities(m modelRoute) modelCapabilities {
	caps := defaultModelCapabilities[strings.ToLower(strings.TrimSpace(m.Model))]
	if m.ContextLength > 0 {
		caps.ContextLength = m.ContextLength
	}
	if m.MaxCompletionTokens > 0 {
		caps.MaxCompletionTokens = m.MaxCompletionTokens
	}
	if len(m.InputModalities) > 0 {
		caps.InputModalities = m.InputModalities
	}
	if len(m.ReasoningLevels) > 0 {
		caps.Thinking = &thinkingSupport{
			ZeroAllowed:    true,
			DynamicAllowed: true,
			Levels:         m.ReasoningLevels,
		}
	}
	return caps
}

// discoveredZenModels returns the cached free-tier model IDs fetched from
// the zen /models endpoint, refreshing synchronously (at most once per TTL)
// when a key is known. Discovery failures silently fall back to the cache.
var discoveredModels struct {
	mu        sync.RWMutex
	ids       []string
	fetchedAt time.Time
}

// seenAuthKeys accumulates zen API keys observed by auth.parse; discovery
// needs one to query /models.
var seenAuthKeys struct {
	mu   sync.Mutex
	keys []string
}

func rememberAuthKey(key string) {
	if strings.TrimSpace(key) == "" {
		return
	}
	seenAuthKeys.mu.Lock()
	defer seenAuthKeys.mu.Unlock()
	for _, k := range seenAuthKeys.keys {
		if k == key {
			return
		}
	}
	seenAuthKeys.keys = append(seenAuthKeys.keys, key)
}

func anyKnownKey(cfg pluginConfig) string {
	if len(cfg.APIKeys) > 0 {
		return cfg.APIKeys[0]
	}
	seenAuthKeys.mu.Lock()
	defer seenAuthKeys.mu.Unlock()
	if len(seenAuthKeys.keys) > 0 {
		return seenAuthKeys.keys[0]
	}
	return ""
}

// authBaseURL picks the base URL for discovery: the auth file's custom
// service address if present, else plugin config, else the zen default.
func authBaseURL(stored map[string]any, cfg pluginConfig) string {
	for _, field := range []string{"base_url", "url"} {
		if v, ok := stored[field].(string); ok {
			if u := strings.TrimRight(strings.TrimSpace(v), "/"); u != "" {
				return u
			}
		}
	}
	if cfg.BaseURL != "" {
		return cfg.BaseURL
	}
	return zenBaseURL
}

// fetchZenModels lists free-tier model IDs from the zen /models endpoint.
// ponytail: plain http.Client instead of host.http.do — model.register has no
// host callback context; add host-routed discovery if proxy support matters.
func fetchZenModels(baseURL, apiKey string) ([]string, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodGet, baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("zen /models status %d: %s", resp.StatusCode, truncate(body, 256))
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(list.Data))
	for _, m := range list.Data {
		if id := strings.TrimSpace(m.ID); id != "" && strings.HasSuffix(strings.ToLower(id), "-free") {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// refreshDiscoveredModels updates the discovery cache (best effort).
func refreshDiscoveredModels(baseURL, apiKey string) {
	ids, err := fetchZenModels(baseURL, apiKey)
	if err != nil {
		return
	}
	discoveredModels.mu.Lock()
	discoveredModels.ids, discoveredModels.fetchedAt = ids, time.Now()
	discoveredModels.mu.Unlock()
}

// discoveredZenModels returns the cached free-tier IDs, refreshing the cache
// synchronously at most once per TTL when a key is known.
func discoveredZenModels(cfg pluginConfig) []string {
	discoveredModels.mu.RLock()
	fresh := time.Since(discoveredModels.fetchedAt) < 10*time.Minute
	ids := discoveredModels.ids
	discoveredModels.mu.RUnlock()
	if fresh {
		return ids
	}
	key := anyKnownKey(cfg)
	if key == "" {
		return ids
	}
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = zenBaseURL
	}
	refreshDiscoveredModels(baseURL, key)
	discoveredModels.mu.RLock()
	defer discoveredModels.mu.RUnlock()
	return discoveredModels.ids
}

// modelExcluded reports whether a discovered model ID is filtered out by
// the exclude-models plugin configuration (case-insensitive).
func modelExcluded(cfg pluginConfig, model string) bool {
	for _, ex := range cfg.ExcludeModels {
		if strings.EqualFold(strings.TrimSpace(ex), model) {
			return true
		}
	}
	return false
}

// modelRegistration announces supported Zen models to CPA.
// If the user configured custom models in plugins.configs.zen, it announces those;
// otherwise it announces the default set of Zen free-tier models plus any
// free-tier models discovered live from the zen /models endpoint.
func modelRegistration() ([]byte, error) {
	cfg := loadedConfig()
	declared := cfg.Models
	if len(declared) == 0 {
		declared = defaultZenModels
		for _, id := range discoveredZenModels(cfg) {
			if modelExcluded(cfg, id) {
				continue
			}
			dup := false
			for _, m := range declared {
				if strings.EqualFold(m.Model, id) {
					dup = true
					break
				}
			}
			if !dup {
				// Endpoint is a guess; the 400-retry in execute()/runStream()
				// self-corrects chat vs responses per model.
				declared = append(declared, modelRoute{Model: id, Alias: id, Endpoint: inferEndpoint(id)})
			}
		}
	}
	models := make([]modelInfo, 0, len(declared))
	for _, m := range declared {
		if m.Model == "" {
			continue
		}
		// configure() and defaultZenModels guarantee Alias is non-empty.
		alias := m.Alias
		caps := resolveModelCapabilities(m)
		models = append(models, modelInfo{
			ID:                       alias,
			Object:                   "model",
			Created:                  1735689600,
			OwnedBy:                  cfg.Provider,
			Type:                     "openai",
			DisplayName:              m.Model,
			ContextLength:            caps.ContextLength,
			MaxCompletionTokens:      caps.MaxCompletionTokens,
			SupportedInputModalities: caps.InputModalities,
			Thinking:                 caps.Thinking,
		})
	}
	return okEnvelopeJSON(modelRegistrationResponse{
		Provider: cfg.Provider,
		Models:   models,
	})
}

// ---------------------------------------------------------------------------
// execution
// ---------------------------------------------------------------------------

// execute implements executor.execute / executor.execute_stream.
//
// The host translates the client request into one of the declared input
// formats (chat-completions or responses) before calling us, and translates
// our output back to the client protocol. We:
//
//  1. resolve the model route (endpoint split: muse-spark → /responses,
//     chat-only free models → /chat/completions)
//  2. apply the zen free-tier gate (canonical ses_/msg_ ids, identity
//     headers, bash/read tools, stream:true)
//  3. POST to zen via the host HTTP client (proxy + request-log aware)
//  4. stream SSE chunks back through the plugin stream bridge, or fold the
//     whole SSE answer into one JSON payload for non-streaming clients.
func execute(payload []byte, stream bool) ([]byte, error) {
	var req executorRequest
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
	}
	cfg := loadedConfig()
	if !cfg.Enabled {
		return nil, fmt.Errorf("zen provider plugin is disabled")
	}
	route, ok := routeForModel(cfg, req.Model)
	if !ok {
		return nil, fmt.Errorf("zen provider has no model %q", req.Model)
	}

	apiKey := apiKeyForRequest(req, cfg)
	if apiKey == "" {
		return nil, fmt.Errorf("zen provider has no api-key for request (please configure in AI Provider panel or plugins.configs.zen)")
	}

	baseURL := baseURLForRequest(req, cfg)
	headers := gateHeaders(req)

	if !stream {
		return executeNonStream(req, cfg, route, baseURL, headers, apiKey)
	}

	streamID := strings.TrimSpace(req.StreamID)
	if streamID == "" {
		return nil, fmt.Errorf("stream_id is required for executor.execute_stream")
	}
	go runStream(req, cfg, route, baseURL, headers, apiKey, streamID)
	return okEnvelopeJSON(executorStreamResponse{
		Headers: map[string][]string{"Content-Type": {"text/event-stream"}},
	})
}

// executeNonStream streams from zen and folds the SSE answer into one JSON
// payload. When the model has no explicit endpoint config and /chat/completions
// answers 400, it retries once against /responses and remembers the endpoint.
func executeNonStream(req executorRequest, cfg pluginConfig, route modelRoute, baseURL string, headers map[string][]string, apiKey string) ([]byte, error) {
	endpoints := []string{route.Endpoint}
	if route.Endpoint == "chat" && !routeConfigured(cfg, route.Model) {
		endpoints = append(endpoints, "responses")
	}
	var lastErr error
	for _, ep := range endpoints {
		r := route
		r.Endpoint = ep
		// Gate rule 4: zen only answers streaming requests; the SSE answer
		// is folded below.
		upstreamBody, err := prepareUpstreamBody(req, r)
		if err != nil {
			return nil, err
		}
		body, respHeaders, status, err := doUpstream(req.HostCallbackID, http.MethodPost, baseURL+r.EndpointPath(), headers, apiKey, upstreamBody)
		if err != nil {
			return nil, err
		}
		if status < 200 || status >= 300 {
			lastErr = fmt.Errorf("zen upstream status %d: %s", status, truncate(body, 512))
			if status == 400 && ep == "chat" && len(endpoints) > 1 {
				continue // model may be responses-only upstream
			}
			return nil, lastErr
		}
		if ep != route.Endpoint {
			rememberEndpoint(route.Model, ep)
		}
		folded, err := foldSSEToJSON(body, ep)
		if err != nil {
			return nil, err
		}
		if ep == "responses" {
			// The plugin declares chat-completions output, so the folded
			// responses object must become a chat.completion before the host
			// translates it for the requesting client.
			folded, err = responsesCompletionToChat(folded, req.Model)
			if err != nil {
				return nil, err
			}
		}
		return okEnvelopeJSON(executorResponse{Payload: folded, Headers: respHeaders})
	}
	return nil, lastErr
}

// baseURLForRequest resolves the upstream base URL. It checks the host-selected
// auth record (StorageJSON) first for any custom service address, falling back to
// plugin config or the default https://opencode.ai/zen/v1.
func baseURLForRequest(req executorRequest, cfg pluginConfig) string {
	if len(req.StorageJSON) > 0 {
		var stored struct {
			BaseURL string `json:"base_url"`
			URL     string `json:"url"`
		}
		if err := json.Unmarshal(req.StorageJSON, &stored); err == nil {
			u := strings.TrimRight(strings.TrimSpace(stored.BaseURL), "/")
			if u == "" {
				u = strings.TrimRight(strings.TrimSpace(stored.URL), "/")
			}
			if u != "" {
				return u
			}
		}
	}
	if cfg.BaseURL != "" {
		return cfg.BaseURL
	}
	return zenBaseURL
}

// apiKeyForRequest resolves the zen API key for an executor request. It
// prefers the key carried by the host-selected auth record (StorageJSON) and
// falls back to the plugin-config rotation when the host provides none.
func apiKeyForRequest(req executorRequest, cfg pluginConfig) string {
	if len(req.StorageJSON) > 0 {
		var stored struct {
			APIKey string `json:"api_key"`
			Key    string `json:"key"`
		}
		if err := json.Unmarshal(req.StorageJSON, &stored); err == nil {
			if k := strings.TrimSpace(stored.APIKey); k != "" {
				return k
			}
			if k := strings.TrimSpace(stored.Key); k != "" {
				return k
			}
		}
	}
	return nextAPIKey(cfg)
}

// runStream performs the upstream call and forwards SSE frames through the
// host stream bridge until done, then closes the plugin stream.
func runStream(req executorRequest, cfg pluginConfig, route modelRoute, baseURL string, headers map[string][]string, apiKey string, streamID string) {
	closeStream := func(errMsg string) {
		_, _ = callHost("host.stream.close", map[string]any{"stream_id": streamID, "error": strings.TrimSpace(errMsg)})
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			closeStream(fmt.Sprintf("zen stream panic: %v", recovered))
		}
	}()

	// Endpoint auto-detection: without explicit config, a 400 from
	// /chat/completions is retried once against /responses and remembered.
	endpoints := []string{route.Endpoint}
	if route.Endpoint == "chat" && !routeConfigured(cfg, route.Model) {
		endpoints = append(endpoints, "responses")
	}
	var resp *hostStreamHandle
	usedEndpoint := route.Endpoint
	for _, ep := range endpoints {
		r := route
		r.Endpoint = ep
		// Gate rule 4: zen only answers streaming requests.
		body, err := prepareUpstreamBody(req, r)
		if err != nil {
			closeStream(err.Error())
			return
		}
		resp, err = doUpstreamStream(req.HostCallbackID, http.MethodPost, baseURL+r.EndpointPath(), headers, apiKey, body)
		if err != nil {
			closeStream(err.Error())
			return
		}
		usedEndpoint = ep
		if resp.StatusCode == 400 && ep == "chat" && len(endpoints) > 1 {
			// Drain the error body so the abandoned host stream is reaped.
			for i := 0; i < 8; i++ {
				chunk, errRead := readHostStream(resp.StreamID)
				if errRead != nil || chunk.Done || chunk.Error != "" {
					break
				}
			}
			continue
		}
		break
	}
	if resp == nil {
		closeStream("zen upstream: no endpoint attempted")
		return
	}
	if usedEndpoint != route.Endpoint {
		rememberEndpoint(route.Model, usedEndpoint)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Drain a bit of the error body for the message, then close.
		errBody := ""
		if chunk, errRead := readHostStream(resp.StreamID); errRead == nil && len(chunk.Payload) > 0 {
			errBody = truncate(chunk.Payload, 512)
		}
		closeStream(fmt.Sprintf("zen upstream status %d: %s", resp.StatusCode, errBody))
		return
	}

	// Chat-completions clients (SourceFormat "openai" / "chat-completions") get
	// their SSE framing applied by the host itself: when outputFormat equals the
	// requested format the host passes plugin chunks through verbatim and the
	// handler writes the "data: " prefix, so pre-framed lines would double it.
	// Responses/claude/gemini clients go through TranslateStream, whose
	// translators expect "data:" framed input and strip the prefix themselves.
	src := strings.ToLower(strings.TrimSpace(req.SourceFormat))
	rawData := src == "openai" || strings.Contains(src, "chat")

	emit := func(frame []byte) error { return emitStreamFrame(streamID, frame) }
	if route.Endpoint == "responses" {
		// The plugin declares chat-completions output, so the host expects chat
		// chunks; upstream /responses events must be converted first.
		conv := newResponsesChatStreamConverter(req.Model)
		emit = func(frame []byte) error {
			payload := stripSSEDataFraming(frame)
			outs, err := conv.convert([]byte(payload))
			if err != nil {
				return err
			}
			for _, out := range outs {
				framed := out
				if !rawData {
					framed = []byte("data: " + string(out) + "\n\n")
				}
				if err := emitStreamFrame(streamID, framed); err != nil {
					return err
				}
			}
			return nil
		}
	}

	reassembler := &sseReassembler{
		rawData: rawData,
		emit:    emit,
	}
	for {
		chunk, err := readHostStream(resp.StreamID)
		if err != nil {
			closeStream(err.Error())
			return
		}
		if chunk.Error != "" {
			closeStream(chunk.Error)
			return
		}
		// Process the payload before honoring Done: the host bridge may
		// deliver the final body bytes together with the done marker in one
		// chunk, and dropping them would truncate the tail of the answer.
		if len(chunk.Payload) > 0 {
			if err := reassembler.write(chunk.Payload); err != nil {
				closeStream(err.Error())
				return
			}
		}
		if chunk.Done {
			// Flush any partial line and pending event before closing.
			if err := reassembler.flush(); err != nil {
				closeStream(err.Error())
				return
			}
			break
		}
	}
	closeStream("")
}

// ---------------------------------------------------------------------------
// gate emulation
// ---------------------------------------------------------------------------

// gateHeaders builds the upstream header set for the zen free-tier gate.
func gateHeaders(req executorRequest) map[string][]string {
	cfg := loadedConfig()
	headers := map[string][]string{
		"User-Agent":        {defaultUserAgent},
		"Accept":            {"*/*"},
		"Content-Type":      {"application/json"},
		targetProjectHeader: {cfg.Project},
		targetRequestHeader: {canonicalID("msg", requestIdentity(req))},
	}
	if v, ok := headerValue(req.Headers, targetClientHeader); ok && v != "" {
		headers[targetClientHeader] = []string{v}
	} else {
		headers[targetClientHeader] = []string{cfg.Client}
	}
	if session, ok := resolveSession(&req); ok {
		headers[targetSessionHeader] = []string{session}
	}
	return headers
}

// requestIdentity derives a stable per-execution identity for the msg_ id.
func requestIdentity(req executorRequest) string {
	if id, ok := req.Metadata["request_id"].(string); ok && strings.TrimSpace(id) != "" {
		return id
	}
	return fmt.Sprintf("%s\x00%s", req.Model, string(req.Payload))
}

// resolveSession returns the canonical ses_ id for this conversation.
// Priority: existing canonical client value → first present client session
// header → metadata canonical_session_id → content-derived fallback.
func resolveSession(req *executorRequest) (string, bool) {
	if existing, exists := headerValue(req.Headers, targetSessionHeader); exists {
		if existing == "" {
			return "", false
		}
		if canonicalSessionRe.MatchString(existing) {
			return existing, true
		}
		return canonicalID("ses", existing), true
	}
	for _, src := range sessionSources {
		if v, exists := headerValue(req.Headers, src); exists {
			if v == "" {
				return "", false
			}
			return canonicalID("ses", v), true
		}
	}
	if src := sessionFallback(req.Metadata); src != "" {
		return canonicalID("ses", src), true
	}
	if src := contentFallback(req); src != "" {
		return canonicalID("ses", src), true
	}
	return "", false
}

func sessionFallback(meta map[string]any) string {
	if meta == nil {
		return ""
	}
	s, ok := meta["canonical_session_id"].(string)
	if !ok {
		return ""
	}
	normalized, ok := normalizeSessionID(s)
	if !ok {
		return ""
	}
	return normalized
}

// contentFallback derives a stable conversation key from the model plus the
// first user message (stateless, like the host's canonical_session_id).
func contentFallback(req *executorRequest) string {
	text := firstUserText(req.Payload)
	if text == "" {
		return ""
	}
	return "content:" + req.Model + "\x00" + text
}

// ---------------------------------------------------------------------------
// upstream body preparation
// ---------------------------------------------------------------------------

// prepareUpstreamBody applies the body-level gate rules to the (already
// host-translated) payload:
//   - "stream": true (zen rejects stream:false with 403 FreeTierError)
//   - tools contains bash + read in the dialect of the payload
func prepareUpstreamBody(req executorRequest, route modelRoute) ([]byte, error) {
	body := req.Payload
	if len(body) == 0 {
		body = req.OriginalRequest
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" || trimmed[0] != '{' {
		return nil, fmt.Errorf("zen executor: unsupported payload shape")
	}
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, fmt.Errorf("zen executor: payload is not valid JSON: %w", err)
	}

	// The host translated the request into chat-completions or responses
	// format; keep the model pointing at the configured upstream name.
	if route.Model != "" {
		root["model"] = route.Model
	}
	if err := convertBodyDialect(root, route.Endpoint); err != nil {
		return nil, err
	}
	// Repair host-translated payloads before they reach zen: the host's
	// protocol translation can emit shapes zen's strict validator rejects
	// with 400 (text parts without a usable "text" field, roleless messages
	// from reasoning items, null content).
	sanitizeChatMessages(root)
	root["stream"] = true

	dialect, known := bodyDialect(root)
	if known {
		ensureGateTools(root, dialect)
	}

	out, err := json.Marshal(root)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// sanitizeChatMessages repairs a chat-completions payload in place so zen's
// strict validator never sees malformed content. It drops messages without a
// usable role (artifacts of translated reasoning items), drops content parts
// whose declared text is missing or empty, and normalizes null content to ""
// (zen rejects null content). Valid parts arrays are preserved as arrays.
func sanitizeChatMessages(root map[string]any) {
	items, ok := root["messages"].([]any)
	if !ok {
		return
	}
	out := make([]any, 0, len(items))
	for _, item := range items {
		msg, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role = strings.TrimSpace(role); role == "" {
			continue
		}
		msg["role"] = role
		switch content := msg["content"].(type) {
		case []any:
			kept := make([]any, 0, len(content))
			for _, p := range content {
				part, ok := p.(map[string]any)
				if !ok {
					continue
				}
				partType, _ := part["type"].(string)
				switch partType {
				case "text", "input_text", "output_text":
					text, ok := part["text"].(string)
					if !ok || strings.TrimSpace(text) == "" {
						continue
					}
					part["type"] = "text"
					delete(part, "annotations")
					kept = append(kept, part)
				default:
					kept = append(kept, part)
				}
			}
			if len(kept) == 0 {
				msg["content"] = ""
			} else {
				msg["content"] = kept
			}
		case nil:
			msg["content"] = ""
		}
		out = append(out, msg)
	}
	if len(out) > 0 {
		root["messages"] = out
	}
}

// convertBodyDialect converts a host-translated request between the
// chat-completions and responses payload shapes. CPA may hand us either
// format regardless of the endpoint a model requires, so the upstream body
// must always match route.Endpoint.
func convertBodyDialect(root map[string]any, endpoint string) error {
	dialect, known := bodyDialect(root)
	if !known || dialect == endpoint {
		return nil
	}
	if endpoint == "chat" {
		return responsesToChat(root)
	}
	return chatToResponses(root)
}

func responsesToChat(root map[string]any) error {
	input, ok := root["input"]
	if !ok {
		return nil
	}
	delete(root, "input")

	var messages []any
	if instructions, ok := root["instructions"].(string); ok && strings.TrimSpace(instructions) != "" {
		messages = append(messages, map[string]any{
			"role":    "system",
			"content": instructions,
		})
	}
	delete(root, "instructions")

	switch source := input.(type) {
	case string:
		messages = append(messages, map[string]any{"role": "user", "content": source})
	case []any:
		for _, item := range source {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			// Reasoning items have no chat-completions representation. The
			// host's translation of them produces malformed messages, so drop
			// them here when the payload arrives in responses form.
			if itemType, _ := entry["type"].(string); strings.EqualFold(itemType, "reasoning") {
				continue
			}
			message := map[string]any{"role": entry["role"]}
			if content, ok := entry["content"]; ok {
				message["content"] = renamePartType(content, "text", "input_text", "output_text")
			}
			messages = append(messages, message)
		}
	default:
		return fmt.Errorf("zen executor: unsupported responses input shape")
	}
	if len(messages) == 0 {
		return fmt.Errorf("zen executor: responses input is empty")
	}
	root["messages"] = messages

	if tools, ok := root["tools"].([]any); ok {
		root["tools"] = responsesToolsToChat(tools)
	}
	if text, ok := root["text"].(map[string]any); ok {
		if format, ok := text["format"]; ok {
			root["response_format"] = format
		}
		delete(root, "text")
	}
	return nil
}

// renamePartType rewrites content parts whose "type" matches one of
// fromTypes to toType, copying all other fields verbatim. Content is
// returned unchanged when no part matched.
func renamePartType(content any, toType string, fromTypes ...string) any {
	parts, ok := content.([]any)
	if !ok {
		return content
	}
	out := make([]any, 0, len(parts))
	changed := false
	for _, item := range parts {
		part, ok := item.(map[string]any)
		if !ok {
			out = append(out, item)
			continue
		}
		partType, _ := part["type"].(string)
		matched := false
		for _, want := range fromTypes {
			if partType == want {
				matched = true
				break
			}
		}
		if !matched {
			out = append(out, part)
			continue
		}
		converted := make(map[string]any, len(part))
		for k, v := range part {
			if k == "type" {
				converted[k] = toType
			} else {
				converted[k] = v
			}
		}
		out = append(out, converted)
		changed = true
	}
	if !changed {
		return content
	}
	return out
}

func responsesToolsToChat(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, item := range tools {
		tool, ok := item.(map[string]any)
		if !ok {
			out = append(out, item)
			continue
		}
		if fn, ok := tool["function"].(map[string]any); ok {
			for k, v := range fn {
				if v == nil {
					delete(fn, k)
				}
			}
			out = append(out, tool)
			continue
		}
		name, _ := tool["name"].(string)
		if name == "" {
			out = append(out, tool)
			continue
		}
		fn, ok := tool["function"].(map[string]any)
		if !ok {
			fn = make(map[string]any, len(tool))
			for k, v := range tool {
				if k == "type" {
					continue
				}
				fn[k] = v
			}
		}
		// zen rejects "strict": null (pi sends it on every tool); drop null-valued keys.
		for k, v := range fn {
			if v == nil {
				delete(fn, k)
			}
		}
		converted := map[string]any{"type": "function", "function": fn}
		out = append(out, converted)
	}
	return out
}

func chatToResponses(root map[string]any) error {
	source, ok := root["messages"].([]any)
	if !ok {
		return nil
	}
	delete(root, "messages")

	input := make([]any, 0, len(source))
	for _, item := range source {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		message := map[string]any{"role": entry["role"]}
		if content, ok := entry["content"]; ok {
			message["content"] = renamePartType(content, "input_text", "text")
		}
		input = append(input, message)
	}
	if len(input) == 0 {
		return fmt.Errorf("zen executor: chat messages are empty")
	}
	root["input"] = input

	if tools, ok := root["tools"].([]any); ok {
		root["tools"] = chatToolsToResponses(tools)
	}
	if format, ok := root["response_format"]; ok {
		root["text"] = map[string]any{"format": format}
		delete(root, "response_format")
	}
	return nil
}

func chatToolsToResponses(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, item := range tools {
		tool, ok := item.(map[string]any)
		if !ok {
			out = append(out, item)
			continue
		}
		fn, ok := tool["function"].(map[string]any)
		if !ok {
			out = append(out, tool)
			continue
		}
		converted := make(map[string]any, len(fn)+1)
		for k, v := range fn {
			converted[k] = v
		}
		converted["type"] = "function"
		out = append(out, converted)
	}
	return out
}

func bodyDialect(root map[string]any) (string, bool) {
	if _, ok := root["messages"]; ok {
		return "chat", true
	}
	if _, ok := root["input"]; ok {
		return "responses", true
	}
	return "", false
}

// ensureGateTools appends minimal bash/read tool entries when missing, in
// the dialect-appropriate shape.
func ensureGateTools(root map[string]any, dialect string) {
	tools, _ := root["tools"].([]any)
	names := map[string]bool{}
	for _, item := range tools {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		names[toolEntryName(entry, dialect)] = true
	}
	var missing []string
	for _, want := range []string{"bash", "read"} {
		if !names[want] {
			missing = append(missing, want)
		}
	}
	if len(missing) == 0 {
		return
	}
	for _, name := range missing {
		tools = append(tools, gateToolEntry(name, dialect))
	}
	root["tools"] = tools
}

func toolEntryName(entry map[string]any, dialect string) string {
	if dialect == "chat" {
		if fn, ok := entry["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok {
				return strings.TrimSpace(name)
			}
		}
		return ""
	}
	if name, ok := entry["name"].(string); ok {
		return strings.TrimSpace(name)
	}
	return ""
}

func gateToolEntry(name, dialect string) map[string]any {
	schema := map[string]any{"type": "object"}
	description := "Runs a persistent bash shell session."
	if name == "read" {
		description = "Reads a file from the local filesystem."
	}
	if dialect == "chat" {
		return map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": description,
				"parameters":  schema,
			},
		}
	}
	return map[string]any{
		"type":        "function",
		"name":        name,
		"description": description,
		"parameters":  schema,
	}
}

// firstUserText extracts the first user message text for the content-derived
// session fallback. Understands both chat and responses payloads.
func firstUserText(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" || trimmed[0] != '{' {
		return ""
	}
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return ""
	}
	if items, ok := root["messages"].([]any); ok {
		return firstUserTextFromItems(items)
	}
	switch input := root["input"].(type) {
	case string:
		return input
	case []any:
		return firstUserTextFromItems(input)
	}
	return ""
}

// firstUserTextFromItems returns the text of the first user message in a
// chat-style message list (string content or text parts).
func firstUserTextFromItems(items []any) string {
	for _, item := range items {
		msg, ok := item.(map[string]any)
		if !ok || msg["role"] != "user" {
			continue
		}
		if s, ok := msg["content"].(string); ok && s != "" {
			return s
		}
		if parts, ok := msg["content"].([]any); ok {
			for _, part := range parts {
				pm, ok := part.(map[string]any)
				if !ok {
					continue
				}
				if s, ok := pm["text"].(string); ok && s != "" {
					return s
				}
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// canonical id minting
// ---------------------------------------------------------------------------

// canonicalID deterministically maps an arbitrary source string onto zen's
// canonical id shapes (ses_/msg_ + 12 hex + 14 alphanumerics, 30 chars).
func canonicalID(prefix, source string) string {
	sum := sha256.Sum256([]byte(source))
	hexPart := hex.EncodeToString(sum[:6])
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	suffix := make([]byte, 14)
	for i := 0; i < 14; i++ {
		suffix[i] = alphabet[int(sum[6+i])%len(alphabet)]
	}
	return prefix + "_" + hexPart + string(suffix)
}

// ---------------------------------------------------------------------------
// header helpers
// ---------------------------------------------------------------------------

// headerValue returns the trimmed value for a header name and whether the
// header was present. Conflicting repeated or case-variant values fail
// closed (present, empty).
func headerValue(headers map[string][]string, name string) (string, bool) {
	if headers == nil {
		return "", false
	}
	var found bool
	var value string
	for key, values := range headers {
		if !strings.EqualFold(key, name) {
			continue
		}
		for _, v := range values {
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			if found && v != value {
				return "", true
			}
			if !found {
				found = true
				value = v
			}
		}
	}
	if !found {
		return "", false
	}
	normalized, ok := normalizeSessionID(value)
	if !ok {
		return "", true
	}
	return normalized, true
}

// normalizeSessionID trims, enforces the size cap, and rejects Unicode
// control characters.
func normalizeSessionID(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxSessionIDBytes {
		return "", false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return "", false
		}
	}
	return value, true
}

// ---------------------------------------------------------------------------
// host callbacks (HTTP + stream bridge)
// ---------------------------------------------------------------------------

type hostHTTPResponse struct {
	StatusCode int                 `json:"StatusCode"`
	Headers    map[string][]string `json:"Headers"`
	Body       []byte              `json:"Body"`
}

type hostHTTPStreamResponse struct {
	StatusCode int                 `json:"status_code"`
	Headers    map[string][]string `json:"headers,omitempty"`
	StreamID   string              `json:"stream_id,omitempty"`
}

// callHost invokes a host callback method and decodes the result envelope.
func callHost(method string, payload any) (json.RawMessage, error) {
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal host callback payload %s: %w", method, err)
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var response C.cliproxy_buffer
	var requestPtr *C.uint8_t
	if len(rawPayload) > 0 {
		cPayload := C.CBytes(rawPayload)
		if cPayload == nil {
			return nil, fmt.Errorf("allocate host callback payload %s", method)
		}
		defer C.free(cPayload)
		requestPtr = (*C.uint8_t)(cPayload)
	}
	callCode := C.call_host_api(cMethod, requestPtr, C.size_t(len(rawPayload)), &response)
	var rawResponse []byte
	if response.ptr != nil && response.len > 0 {
		rawResponse = C.GoBytes(response.ptr, C.int(response.len))
	}
	if response.ptr != nil {
		C.free_host_buffer(response.ptr, response.len)
	}
	if len(rawResponse) == 0 {
		return nil, fmt.Errorf("host callback %s returned no response, code=%d", method, int(callCode))
	}
	var env envelope
	if err := json.Unmarshal(rawResponse, &env); err != nil {
		return nil, fmt.Errorf("decode host callback envelope %s: %w", method, err)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("%s: %s", env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("host callback %s failed", method)
	}
	return append(json.RawMessage(nil), env.Result...), nil
}

// keyRotation rotates zen API keys round-robin.
var keyRotation struct {
	mu    sync.Mutex
	index int
}

func nextAPIKey(cfg pluginConfig) string {
	if len(cfg.APIKeys) == 0 {
		return ""
	}
	keyRotation.mu.Lock()
	defer keyRotation.mu.Unlock()
	key := cfg.APIKeys[keyRotation.index%len(cfg.APIKeys)]
	keyRotation.index++
	return key
}

// doUpstream performs a blocking host HTTP call.
func doUpstream(hostCallbackID, method, url string, headers map[string][]string, apiKey string, body []byte) ([]byte, map[string][]string, int, error) {
	req := map[string]any{
		"Method":  method,
		"URL":     url,
		"Headers": withAuth(headers, apiKey),
		"Body":    body,
	}
	if hostCallbackID != "" {
		req["host_callback_id"] = hostCallbackID
	}
	raw, err := callHost("host.http.do", req)
	if err != nil {
		return nil, nil, 0, err
	}
	var resp hostHTTPResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, nil, 0, err
	}
	return resp.Body, resp.Headers, resp.StatusCode, nil
}

// hostStreamHandle is an opened host HTTP stream.
type hostStreamHandle struct {
	StatusCode int
	Headers    map[string][]string
	StreamID   string
}

// doUpstreamStream opens a host HTTP stream and returns immediately.
func doUpstreamStream(hostCallbackID, method, url string, headers map[string][]string, apiKey string, body []byte) (*hostStreamHandle, error) {
	req := map[string]any{
		"Method":  method,
		"URL":     url,
		"Headers": withAuth(headers, apiKey),
		"Body":    body,
	}
	if hostCallbackID != "" {
		req["host_callback_id"] = hostCallbackID
	}
	raw, err := callHost("host.http.do_stream", req)
	if err != nil {
		return nil, err
	}
	var resp hostHTTPStreamResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	if resp.StreamID == "" {
		return nil, fmt.Errorf("host http stream bridge is unavailable")
	}
	return &hostStreamHandle{
		StatusCode: resp.StatusCode,
		Headers:    resp.Headers,
		StreamID:   resp.StreamID,
	}, nil
}

// readHostStream reads the next chunk from an open host HTTP stream.
func readHostStream(streamID string) (httpStreamChunk, error) {
	raw, err := callHost("host.http.stream_read", map[string]any{"stream_id": streamID})
	if err != nil {
		return httpStreamChunk{}, err
	}
	var chunk httpStreamChunk
	if err := json.Unmarshal(raw, &chunk); err != nil {
		return httpStreamChunk{}, err
	}
	return chunk, nil
}

// emitStreamFrame forwards one prepared payload frame through the plugin
// stream bridge. "[DONE]" terminators are dropped: the host appends its own
// done tail after the plugin stream closes (adapters_executors.go
// emitTranslatedExecutorStreamTail / executorStreamDonePayload), so
// forwarding the upstream terminator would duplicate it.
func emitStreamFrame(streamID string, frame []byte) error {
	if len(frame) == 0 {
		return nil
	}
	if isDoneTerminator(string(frame)) {
		return nil
	}
	_, err := callHost("host.stream.emit", map[string]any{
		"stream_id": streamID,
		"payload":   frame,
	})
	return err
}

// sseReassembler turns chunked upstream SSE bytes into complete plugin
// stream emissions. It buffers partial lines across chunk boundaries and
// reassembles the data fields of one event per the SSE specification
// (multiple "data:" lines of an event are joined with "\n") before
// emitting, so a payload split across several data lines reaches the
// client as one frame instead of invalid fragments.
type sseReassembler struct {
	rawData bool
	lineBuf bytes.Buffer
	pending []string // data payloads of the event currently being assembled
	emit    func(frame []byte) error
}

// write feeds one upstream chunk into the reassembler.
func (r *sseReassembler) write(p []byte) error {
	r.lineBuf.Write(p)
	for {
		data := r.lineBuf.Bytes()
		idx := bytes.IndexByte(data, '\n')
		if idx < 0 {
			return nil
		}
		line := make([]byte, idx)
		copy(line, data[:idx])
		r.lineBuf.Next(idx + 1)
		if err := r.handleLine(line); err != nil {
			return err
		}
	}
}

// flush emits any buffered partial line and the pending event. It must be
// called once the upstream stream is finished so the tail of the answer is
// never dropped.
func (r *sseReassembler) flush() error {
	if r.lineBuf.Len() > 0 {
		line := make([]byte, r.lineBuf.Len())
		copy(line, r.lineBuf.Bytes())
		r.lineBuf.Reset()
		if err := r.handleLine(line); err != nil {
			return err
		}
	}
	return r.flushPending()
}

// handleLine processes one complete SSE line.
func (r *sseReassembler) handleLine(line []byte) error {
	s := strings.TrimSpace(strings.TrimRight(string(line), "\r"))
	if s == "" {
		// A blank line terminates the current event.
		return r.flushPending()
	}
	if after, ok := strings.CutPrefix(s, "data:"); ok {
		payload := strings.TrimLeft(after, " \t")
		if payload == "" || strings.HasPrefix(payload, ":") {
			return nil
		}
		r.pending = append(r.pending, payload)
		return nil
	}
	// Non-data lines flush the pending event first to preserve ordering,
	// then keep the single-line passthrough behavior (comments dropped,
	// event/id/retry and bare JSON lines normalized).
	if err := r.flushPending(); err != nil {
		return err
	}
	frame := normalizeSSEFrame(r.rawData, []byte(s))
	if len(frame) == 0 || isDoneTerminator(string(frame)) {
		return nil
	}
	return r.emit(frame)
}

// flushPending emits the joined data payload of the current event.
// "[DONE]" terminators are dropped: the host appends its own done tail
// after the plugin stream closes, so forwarding the upstream terminator
// would duplicate it.
func (r *sseReassembler) flushPending() error {
	if len(r.pending) == 0 {
		return nil
	}
	joined := strings.Join(r.pending, "\n")
	r.pending = nil
	if isDoneTerminator(joined) {
		return nil
	}
	if r.rawData {
		return r.emit([]byte(joined))
	}
	return r.emit([]byte("data: " + joined + "\n\n"))
}

// isDoneTerminator reports whether a payload is an SSE [DONE] terminator.
func isDoneTerminator(s string) bool {
	s = strings.TrimSpace(s)
	return s == "[DONE]" || s == "data: [DONE]"
}

// normalizeSSEFrame converts one non-data upstream SSE line into the
// payload to forward through the host stream bridge. Empty lines and
// keep-alive comments are dropped; event/id/retry lines pass through framed
// (dropped in raw mode); a bare JSON object line is treated as an unframed
// data payload. data: lines and [DONE] terminators are handled by the
// caller (sseReassembler.handleLine).
func normalizeSSEFrame(rawData bool, raw []byte) []byte {
	s := strings.TrimSpace(string(raw))
	if s == "" || strings.HasPrefix(s, ":") {
		return nil
	}
	if strings.HasPrefix(s, "event:") || strings.HasPrefix(s, "id:") || strings.HasPrefix(s, "retry:") {
		if rawData {
			return nil
		}
		return []byte(s + "\n\n")
	}
	if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
		if rawData {
			return []byte(s)
		}
		return []byte("data: " + s + "\n\n")
	}
	return nil
}

func withAuth(headers map[string][]string, apiKey string) map[string][]string {
	out := make(map[string][]string, len(headers)+1)
	for k, v := range headers {
		out[k] = v
	}
	if apiKey != "" {
		out["Authorization"] = []string{"Bearer " + apiKey}
	}
	return out
}

// ---------------------------------------------------------------------------
// SSE folding (non-streaming clients)
// ---------------------------------------------------------------------------

// foldSSEToJSON reassembles an SSE answer into a single JSON payload in the
// dialect of the endpoint (chat.completion / response object).
func foldSSEToJSON(sse []byte, endpoint string) ([]byte, error) {
	switch endpoint {
	case "responses":
		return foldResponsesSSE(sse)
	default:
		return foldChatSSE(sse)
	}
}

// foldChatSSE merges chat.completion.chunk frames into one chat.completion.
func foldChatSSE(sse []byte) ([]byte, error) {
	var (
		id, model, role, finish string
		content                 strings.Builder
		toolCalls               = map[int]*bytes.Buffer{}
		toolNames               = map[int]string{}
		toolOrder               []int
		usage                   json.RawMessage
	)
	for _, frame := range sseFrames(sse) {
		if frame == "[DONE]" {
			break
		}
		var chunk struct {
			ID      string `json:"id"`
			Model   string `json:"model"`
			Choices []struct {
				Index int `json:"index"`
				Delta struct {
					Role      string          `json:"role"`
					Content   string          `json:"content"`
					ToolCalls []toolCallDelta `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(frame), &chunk); err != nil {
			continue // skip non-JSON frames (keep-alives, metadata)
		}
		if chunk.ID != "" {
			id = chunk.ID
		}
		if chunk.Model != "" {
			model = chunk.Model
		}
		if len(chunk.Usage) > 0 && string(chunk.Usage) != "null" {
			usage = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Role != "" {
				role = choice.Delta.Role
			}
			content.WriteString(choice.Delta.Content)
			for _, tc := range choice.Delta.ToolCalls {
				buf := toolCalls[tc.Index]
				if buf == nil {
					buf = &bytes.Buffer{}
					toolCalls[tc.Index] = buf
					toolOrder = append(toolOrder, tc.Index)
				}
				buf.WriteString(tc.Function.Arguments)
				if tc.Function.Name != "" {
					toolNames[tc.Index] = tc.Function.Name
				}
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finish = *choice.FinishReason
			}
		}
	}
	message := map[string]any{"role": role}
	if content.Len() > 0 {
		message["content"] = content.String()
	} else if len(toolCalls) == 0 {
		message["content"] = nil
	}
	if len(toolCalls) > 0 {
		calls := make([]any, 0, len(toolCalls))
		for _, idx := range toolOrder {
			calls = append(calls, map[string]any{
				"id":   fmt.Sprintf("call_%d", idx),
				"type": "function",
				"function": map[string]any{
					"name":      toolNames[idx],
					"arguments": toolCalls[idx].String(),
				},
			})
		}
		message["tool_calls"] = calls
		if finish == "" {
			finish = "tool_calls"
		}
	}
	if finish == "" {
		finish = "stop"
	}
	if role == "" {
		role = "assistant"
	}
	out := map[string]any{
		"id":     id,
		"object": "chat.completion",
		"model":  model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
	}
	if usage != nil {
		out["usage"] = json.RawMessage(usage)
	}
	return json.Marshal(out)
}

type toolCallDelta struct {
	Index    int `json:"index"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// foldResponsesSSE merges response.* events into one response object.
func foldResponsesSSE(sse []byte) ([]byte, error) {
	var (
		id, model, status string
		output            []json.RawMessage
		usage             json.RawMessage
	)
	for _, frame := range sseFrames(sse) {
		if frame == "[DONE]" {
			break
		}
		var ev struct {
			Type   string          `json:"type"`
			ID     string          `json:"id"`
			Model  string          `json:"model"`
			Status string          `json:"status"`
			Item   json.RawMessage `json:"item"`
			Usage  json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(frame), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "response.created":
			if ev.ID != "" {
				id = ev.ID
			}
		case "response.output_item.added":
			if len(ev.Item) > 0 {
				output = append(output, ev.Item)
			}
		case "response.completed":
			// The completed event carries the full response; prefer it.
			var completed struct {
				Response json.RawMessage `json:"response"`
			}
			if err := json.Unmarshal([]byte(frame), &completed); err == nil && len(completed.Response) > 0 {
				return completed.Response, nil
			}
			if len(ev.Usage) > 0 && string(ev.Usage) != "null" {
				usage = ev.Usage
			}
		}
		if ev.Model != "" {
			model = ev.Model
		}
		if ev.Status != "" {
			status = ev.Status
		}
	}
	if status == "" {
		status = "completed"
	}
	out := map[string]any{
		"id":     id,
		"object": "response",
		"status": status,
		"model":  model,
		"output": output,
	}
	if usage != nil {
		out["usage"] = json.RawMessage(usage)
	}
	return json.Marshal(out)
}

// sseFrames splits an SSE byte stream into individual data frame payloads.
func sseFrames(sse []byte) []string {
	var frames []string
	for _, line := range strings.Split(string(sse), "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		frames = append(frames, data)
	}
	return frames
}

// ---------------------------------------------------------------------------
// responses → chat conversion
//
// The plugin declares chat-completions as its executor output format, so the
// host expects chat-format payloads from the plugin and translates them for
// responses/claude/gemini clients itself. Models routed to the /responses
// endpoint (muse-spark) return Responses API events instead, so both the
// stream and the folded non-stream payload must be converted to chat format
// before they reach the host; otherwise the host translators drop the frames
// (stream) or produce garbage (non-stream).
// ---------------------------------------------------------------------------

// responsesChatStreamConverter converts Responses API stream events into
// chat.completion.chunk payloads, statefully.
type responsesChatStreamConverter struct {
	model    string
	id       string
	created  int64
	sawDelta map[string]bool
}

func newResponsesChatStreamConverter(model string) *responsesChatStreamConverter {
	return &responsesChatStreamConverter{model: model, sawDelta: map[string]bool{}}
}

// convert maps one SSE payload (responses event JSON, framing stripped) to
// zero or more chat chunk JSON payloads. A non-JSON payload is dropped.
func (c *responsesChatStreamConverter) convert(payload []byte) ([][]byte, error) {
	var ev struct {
		Type     string `json:"type"`
		ItemID   string `json:"item_id"`
		Delta    string `json:"delta"`
		Response struct {
			ID        string `json:"id"`
			CreatedAt int64  `json:"created_at"`
			Status    string `json:"status"`
			Error     *struct {
				Message string `json:"message"`
			} `json:"error"`
			Usage *struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
				TotalTokens  int64 `json:"total_tokens"`
			} `json:"usage"`
		} `json:"response"`
		Item struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"item"`
	}
	if err := json.Unmarshal(payload, &ev); err != nil {
		return nil, nil
	}
	switch ev.Type {
	case "response.created":
		if ev.Response.ID != "" {
			c.id = ev.Response.ID
		}
		if ev.Response.CreatedAt != 0 {
			c.created = ev.Response.CreatedAt
		}
		return [][]byte{c.chunk(map[string]any{"role": "assistant"}, nil, nil)}, nil
	case "response.output_text.delta":
		if ev.ItemID != "" {
			c.sawDelta[ev.ItemID] = true
		}
		if ev.Delta == "" {
			return nil, nil
		}
		return [][]byte{c.chunk(map[string]any{"content": ev.Delta}, nil, nil)}, nil
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		if ev.ItemID != "" {
			c.sawDelta[ev.ItemID] = true
		}
		if ev.Delta == "" {
			return nil, nil
		}
		return [][]byte{c.chunk(map[string]any{"reasoning_content": ev.Delta}, nil, nil)}, nil
	case "response.output_item.done":
		// Fallback: an item completed without streamed deltas (some upstreams
		// only send the final item). Emit its text once.
		if ev.Item.Type != "message" || ev.Item.ID == "" || c.sawDelta[ev.Item.ID] {
			return nil, nil
		}
		var text strings.Builder
		for _, part := range ev.Item.Content {
			if part.Type == "output_text" || part.Type == "text" {
				text.WriteString(part.Text)
			}
		}
		if text.Len() == 0 {
			return nil, nil
		}
		return [][]byte{c.chunk(map[string]any{"content": text.String()}, nil, nil)}, nil
	case "response.completed":
		var usage map[string]any
		if ev.Response.Usage != nil {
			usage = map[string]any{
				"prompt_tokens":     ev.Response.Usage.InputTokens,
				"completion_tokens": ev.Response.Usage.OutputTokens,
				"total_tokens":      ev.Response.Usage.TotalTokens,
			}
		}
		return [][]byte{c.chunk(map[string]any{}, strPtr("stop"), usage)}, nil
	case "response.failed", "response.incomplete":
		msg := "upstream response " + ev.Type
		if ev.Response.Error != nil && ev.Response.Error.Message != "" {
			msg = ev.Response.Error.Message
		} else if ev.Response.Status != "" {
			msg = "upstream response " + ev.Response.Status
		}
		return nil, fmt.Errorf("zen responses stream failed: %s", msg)
	}
	return nil, nil
}

// chunk renders one chat.completion.chunk payload.
func (c *responsesChatStreamConverter) chunk(delta map[string]any, finish *string, usage map[string]any) []byte {
	id := c.id
	if id == "" {
		id = canonicalID("chatcmpl", c.model)
	}
	created := c.created
	if created == 0 {
		created = time.Now().Unix()
	}
	m := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   c.model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finish,
		}},
	}
	if usage != nil {
		m["usage"] = usage
	}
	b, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// responsesCompletionToChat converts a folded Responses API response object
// into a chat.completion object for the non-streaming path.
func responsesCompletionToChat(body []byte, model string) ([]byte, error) {
	var resp struct {
		ID        string `json:"id"`
		Model     string `json:"model"`
		Status    string `json:"status"`
		CreatedAt int64  `json:"created_at"`
		Output    []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Summary []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"summary"`
		} `json:"output"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Usage *struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
			TotalTokens  int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("zen responses payload is not valid JSON: %w", err)
	}
	if resp.Status == "failed" || resp.Error != nil {
		msg := "upstream response failed"
		if resp.Error != nil && resp.Error.Message != "" {
			msg = resp.Error.Message
		}
		return nil, fmt.Errorf("zen responses stream failed: %s", msg)
	}
	var content, reasoning strings.Builder
	for _, item := range resp.Output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				if part.Type == "output_text" || part.Type == "text" {
					content.WriteString(part.Text)
				}
			}
		case "reasoning":
			for _, sum := range item.Summary {
				reasoning.WriteString(sum.Text)
			}
		}
	}
	if resp.Model != "" {
		model = resp.Model
	}
	id := resp.ID
	if id == "" {
		id = canonicalID("chatcmpl", model)
	}
	created := resp.CreatedAt
	if created == 0 {
		created = time.Now().Unix()
	}
	message := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	m := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": "stop",
		}},
	}
	if resp.Usage != nil {
		m["usage"] = map[string]any{
			"prompt_tokens":     resp.Usage.InputTokens,
			"completion_tokens": resp.Usage.OutputTokens,
			"total_tokens":      resp.Usage.TotalTokens,
		}
	}
	return json.Marshal(m)
}

// stripSSEDataFraming removes the "data:" prefix and surrounding whitespace
// from one emitted SSE frame, yielding the raw payload.
func stripSSEDataFraming(frame []byte) string {
	s := strings.TrimSpace(string(frame))
	if after, ok := strings.CutPrefix(s, "data:"); ok {
		s = strings.TrimSpace(after)
	}
	return s
}

// strPtr is a tiny helper for optional string fields.
func strPtr(s string) *string { return &s }

func truncate(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// ---------------------------------------------------------------------------
// envelope helpers
// ---------------------------------------------------------------------------

func okEnvelopeJSON(result any) ([]byte, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
