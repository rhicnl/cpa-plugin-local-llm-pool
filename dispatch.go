package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/rhicnl/cli-proxy-api-plugin-local-llm-pool/internal/tracker"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// pluginName is also the plugin ID the host derives from the .so filename, so
// the status resource is served at /v0/resource/plugins/local-llm-pool/status.
const pluginName = "local-llm-pool"

// pluginVersion is reported in the plugin.register metadata. Release builds
// stamp the tag onto it with -ldflags "-X main.pluginVersion=<version>"; local
// builds keep the -dev default so a hand-built library is never mistaken for a
// published one.
var pluginVersion = "0.1.0-dev"

// statusResourcePath is registered relative to the plugin resource base path.
const statusResourcePath = "/status"

// minSchemaVersion is the first host contract carrying request.complete, which
// this plugin needs to release slots.
const minSchemaVersion uint32 = 2

// These metadata keys are copied from sdk/cliproxy/executor/types.go
// (RequestedModelMetadataKey and SelectedAuthMetadataKey). They are declared
// here rather than imported so the plugin keeps a stdlib-only dependency graph
// beyond pluginabi and pluginapi.
const (
	requestedModelMetadataKey = "requested_model"
	selectedAuthMetadataKey   = "selected_auth_id"
)

var state struct {
	mu      sync.Mutex
	tracker *tracker.Tracker
}

// lifecycleRequest is the plugin.register and plugin.reconfigure payload.
// Field names match internal/pluginhost/rpc_schema.go rpcLifecycleRequest.
type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

// registration is the plugin.register and plugin.reconfigure response.
// Field names match internal/pluginhost/rpc_schema.go rpcRegistration.
type registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  capabilities       `json:"capabilities"`
}

// capabilities mirrors the subset of rpcCapabilities this plugin declares.
type capabilities struct {
	Scheduler bool `json:"scheduler"`
	// SchedulerAcrossPriorities is required to fill a whole pool: without it
	// the host only offers candidates from the highest available priority tier,
	// so a lower-priority spill-over backend would never appear as a candidate.
	SchedulerAcrossPriorities bool `json:"scheduler_across_priorities"`
	RequestInterceptor        bool `json:"request_interceptor"`
	RequestLifecyclePlugin    bool `json:"request_lifecycle_plugin"`
	ManagementAPI             bool `json:"management_api"`
}

// managementRegistration is the management.register response. The field name
// matches internal/pluginhost/rpc_schema.go rpcManagementRegistrationResponse;
// the ResourceRoute fields carry no JSON tags in the SDK, so they encode under
// their Go names.
type managementRegistration struct {
	Resources []pluginapi.ResourceRoute `json:"resources,omitempty"`
}

// handleMethod dispatches one host RPC call and always returns a JSON envelope.
func handleMethod(method string, request []byte) []byte {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return configure(request)
	case pluginabi.MethodPluginQuiesce:
		return okEnvelope(struct{}{})
	case pluginabi.MethodPluginShutdown:
		resetState()
		return okEnvelope(struct{}{})
	case pluginabi.MethodSchedulerPick:
		return schedulerPick(request)
	case pluginabi.MethodRequestInterceptBefore:
		return passThrough(request)
	case pluginabi.MethodRequestInterceptAfter:
		return interceptAfterAuth(request)
	case pluginabi.MethodRequestComplete:
		return requestComplete(request)
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration{
			Resources: []pluginapi.ResourceRoute{{
				Path:        statusResourcePath,
				Menu:        "Local LLM Pool",
				Description: "Live pool routing plus in-flight and pending slot counts per backend.",
			}},
		})
	case pluginabi.MethodManagementHandle:
		return managementHandle(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method, 0)
	}
}

// resetState drops all live accounting. The host calls plugin.shutdown before
// unloading the library.
func resetState() {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.tracker = nil
}

// configure validates the host-supplied YAML and installs it. An existing
// tracker is reconfigured in place so live in-flight counts survive a reload.
func configure(raw []byte) []byte {
	var req lifecycleRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return errorEnvelope("invalid_request", err.Error(), 0)
		}
	}
	if req.SchemaVersion < minSchemaVersion {
		return errorEnvelope("unsupported_schema", fmt.Sprintf("host schema version %d is older than the required %d", req.SchemaVersion, minSchemaVersion), 0)
	}
	cfg, err := tracker.ParseConfig(req.ConfigYAML)
	if err != nil {
		return errorEnvelope("invalid_config", err.Error(), 0)
	}

	state.mu.Lock()
	if state.tracker == nil {
		state.tracker = tracker.New(cfg, nil)
	} else {
		state.tracker.Reconfigure(cfg)
	}
	state.mu.Unlock()

	return okEnvelope(pluginRegistration())
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           "rhicnl",
			GitHubRepository: "https://github.com/rhicnl/cli-proxy-api-plugin-local-llm-pool",
			ConfigFields: []pluginapi.ConfigField{
				{
					Name:        "group_aliases",
					Type:        pluginapi.ConfigFieldTypeArray,
					Description: "Shorthand single-pool form: model aliases this plugin steers across the configured backends.",
				},
				{
					Name:        "on_saturated",
					Type:        pluginapi.ConfigFieldTypeEnum,
					EnumValues:  []string{string(tracker.PolicyLeastLoaded), string(tracker.PolicyFirst), string(tracker.PolicyReject)},
					Description: "Default behaviour when every backend in a pool is at its concurrency limit. A pool may override it.",
				},
				{
					Name:        "pending_ttl_ms",
					Type:        pluginapi.ConfigFieldTypeInteger,
					Description: "Lifetime of a pick reservation that is never claimed by a request, in milliseconds.",
				},
				{
					Name:        "backends",
					Type:        pluginapi.ConfigFieldTypeArray,
					Description: "Shorthand single-pool form: backends in fill order, each with a match set and a max_concurrency.",
				},
				{
					Name:        "pools",
					Type:        pluginapi.ConfigFieldTypeArray,
					Description: "Multi-pool form: each pool has its own group_aliases, optional on_saturated, and backends in fill order. Mutually exclusive with the top-level group_aliases and backends.",
				},
			},
		},
		Capabilities: capabilities{
			Scheduler:                 true,
			SchedulerAcrossPriorities: true,
			RequestInterceptor:        true,
			RequestLifecyclePlugin:    true,
			ManagementAPI:             true,
		},
	}
}

func schedulerPick(raw []byte) []byte {
	var req pluginapi.SchedulerPickRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return errorEnvelope("invalid_request", err.Error(), 0)
	}
	active := currentTracker()
	if active == nil {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}

	candidates := make([]tracker.Candidate, 0, len(req.Candidates))
	for _, candidate := range req.Candidates {
		candidates = append(candidates, tracker.Candidate{
			AuthID:     candidate.ID,
			Provider:   candidate.Provider,
			Attributes: candidate.Attributes,
		})
	}

	decision := active.Pick(req.Model, metadataString(req.Options.Metadata, requestedModelMetadataKey), candidates)
	if decision.Rejected {
		return errorEnvelope("local_llm_backends_saturated", "all local LLM backends are at their concurrency limit", http.StatusTooManyRequests)
	}
	if !decision.Handled {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}
	return okEnvelope(pluginapi.SchedulerPickResponse{AuthID: decision.AuthID, Handled: true})
}

// passThrough returns the request unchanged, which is what
// request.intercept_before must do for this plugin.
func passThrough(raw []byte) []byte {
	var req pluginapi.RequestInterceptRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return errorEnvelope("invalid_request", err.Error(), 0)
	}
	return okEnvelope(pluginapi.RequestInterceptResponse{Headers: req.Headers, Body: req.Body})
}

// interceptAfterAuth binds the request to the backend owning the selected auth
// and passes the request through unchanged. Model and RequestedModel are
// forwarded so the tracker can tell steered group traffic, which holds a pick
// reservation, from direct-alias traffic, which does not.
func interceptAfterAuth(raw []byte) []byte {
	var req pluginapi.RequestInterceptRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return errorEnvelope("invalid_request", err.Error(), 0)
	}
	if active := currentTracker(); active != nil {
		active.Bind(req.RequestID, metadataString(req.Metadata, selectedAuthMetadataKey), req.Model, req.RequestedModel)
	}
	return okEnvelope(pluginapi.RequestInterceptResponse{Headers: req.Headers, Body: req.Body})
}

func requestComplete(raw []byte) []byte {
	var completion pluginapi.RequestCompletion
	if err := json.Unmarshal(raw, &completion); err != nil {
		return errorEnvelope("invalid_request", err.Error(), 0)
	}
	if active := currentTracker(); active != nil {
		active.Complete(completion.RequestID)
	}
	return okEnvelope(struct{}{})
}

func managementHandle(raw []byte) []byte {
	var req pluginapi.ManagementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return errorEnvelope("invalid_request", err.Error(), 0)
	}
	if !strings.EqualFold(strings.TrimSpace(req.Method), http.MethodGet) || !strings.HasSuffix(strings.TrimRight(req.Path, "/"), statusResourcePath) {
		return okEnvelope(jsonManagementResponse(http.StatusNotFound, map[string]string{"error": "not found"}))
	}

	active := currentTracker()
	if active == nil {
		return okEnvelope(jsonManagementResponse(http.StatusServiceUnavailable, map[string]string{"error": "plugin is not configured"}))
	}
	return okEnvelope(jsonManagementResponse(http.StatusOK, active.Status()))
}

func jsonManagementResponse(statusCode int, payload any) pluginapi.ManagementResponse {
	body, err := json.Marshal(payload)
	if err != nil {
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusInternalServerError,
			Headers:    http.Header{"Content-Type": {"application/json"}},
			Body:       []byte(`{"error":"encode error"}`),
		}
	}
	return pluginapi.ManagementResponse{
		StatusCode: statusCode,
		Headers:    http.Header{"Content-Type": {"application/json"}},
		Body:       body,
	}
}

func currentTracker() *tracker.Tracker {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.tracker
}

func metadataString(metadata map[string]any, key string) string {
	if len(metadata) == 0 {
		return ""
	}
	value, _ := metadata[key].(string)
	return strings.TrimSpace(value)
}

func okEnvelope(v any) []byte {
	result, err := json.Marshal(v)
	if err != nil {
		return errorEnvelope("encode_error", err.Error(), 0)
	}
	raw, err := json.Marshal(pluginabi.Envelope{OK: true, Result: result})
	if err != nil {
		return errorEnvelope("encode_error", err.Error(), 0)
	}
	return raw
}

func errorEnvelope(code, message string, httpStatus int) []byte {
	raw, err := pluginabi.NewErrorEnvelope(code, message, httpStatus)
	if err != nil {
		return []byte(`{"ok":false,"error":{"code":"encode_error","message":"encode error"}}`)
	}
	return raw
}
