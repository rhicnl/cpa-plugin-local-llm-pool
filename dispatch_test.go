package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/rhicnl/cli-proxy-api-plugin-local-llm-pool/internal/tracker"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The JSON in this file is written out by hand rather than produced by
// marshalling the SDK structs, because the point of these tests is to pin the
// exact wire field names the host uses. pluginapi types carry no JSON tags, so
// the host encodes them under their Go field names; only the pluginhost
// wrapper structs in internal/pluginhost/rpc_schema.go use snake_case tags.

const testConfigYAML = `enabled: true
priority: 1
group_aliases: ["local-llm-group"]
on_saturated: least-loaded
pending_ttl_ms: 5000
backends:
  - match: { compat_name: "gpu-box-a" }
    max_concurrency: 8
  - match: { compat_name: "gpu-box-b" }
    max_concurrency: 2
`

func lifecycleJSON(t *testing.T, configYAML string, schemaVersion uint32) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"config_yaml":    []byte(configYAML),
		"schema_version": schemaVersion,
	})
	if err != nil {
		t.Fatalf("marshal lifecycle request: %v", err)
	}
	return raw
}

// decodeOK asserts the envelope succeeded and returns its result payload.
func decodeOK(t *testing.T, raw []byte) json.RawMessage {
	t.Helper()
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode envelope: %v (%s)", err, raw)
	}
	if !envelope.OK {
		t.Fatalf("envelope reported failure: %s", raw)
	}
	return envelope.Result
}

func decodeErr(t *testing.T, raw []byte) *pluginabi.Error {
	t.Helper()
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode envelope: %v (%s)", err, raw)
	}
	if envelope.OK || envelope.Error == nil {
		t.Fatalf("expected an error envelope, got: %s", raw)
	}
	return envelope.Error
}

func mustRegister(t *testing.T, configYAML string) {
	t.Helper()
	resetState()
	t.Cleanup(resetState)
	decodeOK(t, handleMethod(pluginabi.MethodPluginRegister, lifecycleJSON(t, configYAML, pluginabi.SchemaVersion)))
}

// pickJSON builds a scheduler.pick request using the host field names from
// pluginapi.SchedulerPickRequest.
func pickJSON(model, requestedModel string) []byte {
	return []byte(`{
		"Plugin": {"Name": "local-llm-pool", "Version": "0.1.0", "Author": "rhicnl", "GitHubRepository": "", "Logo": "", "ConfigFields": null},
		"Provider": "openai-compatibility",
		"Providers": ["openai-compatibility"],
		"Model": "` + model + `",
		"Stream": false,
		"Options": {
			"Headers": {"X-Test": ["1"]},
			"Metadata": {"requested_model": "` + requestedModel + `"}
		},
		"Candidates": [
			{"ID": "auth-a", "Provider": "openai-compatibility", "Priority": 1, "Status": "active",
			 "Attributes": {"compat_name": "gpu-box-a", "base_url": "http://gpu-box-a.internal:8000/v1", "provider_key": "openai-compatibility"},
			 "Metadata": {}},
			{"ID": "auth-b", "Provider": "openai-compatibility", "Priority": 2, "Status": "active",
			 "Attributes": {"compat_name": "gpu-box-b", "base_url": "http://gpu-box-b.internal:8000/v1", "provider_key": "openai-compatibility"},
			 "Metadata": {}}
		]
	}`)
}

// interceptJSON builds a request.intercept_before or request.intercept_after
// payload for a steered group request.
func interceptJSON(requestID, selectedAuthID string) []byte {
	return interceptJSONFor(requestID, selectedAuthID, "model-a", "local-llm-group")
}

// interceptJSONFor matches rpcRequestInterceptRequest: the embedded
// pluginapi.RequestInterceptRequest fields plus host_callback_id.
func interceptJSONFor(requestID, selectedAuthID, model, requestedModel string) []byte {
	return []byte(`{
		"RequestID": "` + requestID + `",
		"TraceID": "trace-1",
		"SourceFormat": "openai",
		"ToFormat": "openai",
		"Model": "` + model + `",
		"RequestedModel": "` + requestedModel + `",
		"Stream": false,
		"Headers": {"Content-Type": ["application/json"]},
		"Body": "` + base64.StdEncoding.EncodeToString([]byte(`{"model":"local-llm-group"}`)) + `",
		"Metadata": {"selected_auth_id": "` + selectedAuthID + `", "requested_model": "local-llm-group"},
		"host_callback_id": "cb-1"
	}`)
}

// completionJSON matches rpcRequestCompletion.
func completionJSON(requestID string) []byte {
	return []byte(`{
		"RequestID": "` + requestID + `",
		"TraceID": "trace-1",
		"SourceFormat": "openai",
		"Model": "model-a",
		"RequestedModel": "local-llm-group",
		"Stream": false,
		"Outcome": "succeeded",
		"StatusCode": 200,
		"Error": "",
		"StartedAt": "2026-01-01T00:00:00Z",
		"CompletedAt": "2026-01-01T00:00:01Z",
		"Metadata": {"selected_auth_id": "auth-a"},
		"host_callback_id": "cb-1"
	}`)
}

// managementJSON matches rpcManagementRequest.
func managementJSON(method, path string) []byte {
	return []byte(`{
		"Method": "` + method + `",
		"Path": "` + path + `",
		"Headers": {"Accept": ["application/json"]},
		"Query": {},
		"Body": null,
		"host_callback_id": "cb-1"
	}`)
}

func readStatus(t *testing.T) tracker.Status {
	t.Helper()
	result := decodeOK(t, handleMethod(pluginabi.MethodManagementHandle, managementJSON(http.MethodGet, "/v0/resource/plugins/local-llm-pool/status")))
	var response pluginapi.ManagementResponse
	if err := json.Unmarshal(result, &response); err != nil {
		t.Fatalf("decode management response: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status endpoint returned %d: %s", response.StatusCode, response.Body)
	}
	var status tracker.Status
	if err := json.Unmarshal(response.Body, &status); err != nil {
		t.Fatalf("decode status body: %v (%s)", err, response.Body)
	}
	return status
}

// pickResponse runs one scheduler.pick and decodes it into the SDK type.
func pickResponse(t *testing.T, model, requestedModel string) pluginapi.SchedulerPickResponse {
	t.Helper()
	result := decodeOK(t, handleMethod(pluginabi.MethodSchedulerPick, pickJSON(model, requestedModel)))
	var response pluginapi.SchedulerPickResponse
	if err := json.Unmarshal(result, &response); err != nil {
		t.Fatalf("decode pick result: %v", err)
	}
	return response
}

func TestPluginRegisterEnvelope(t *testing.T) {
	resetState()
	t.Cleanup(resetState)
	result := decodeOK(t, handleMethod(pluginabi.MethodPluginRegister, lifecycleJSON(t, testConfigYAML, pluginabi.SchemaVersion)))

	var wire struct {
		SchemaVersion uint32          `json:"schema_version"`
		Metadata      json.RawMessage `json:"metadata"`
		Capabilities  map[string]bool `json:"capabilities"`
	}
	if err := json.Unmarshal(result, &wire); err != nil {
		t.Fatalf("decode registration: %v (%s)", err, result)
	}
	if wire.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf("schema_version = %d, want %d", wire.SchemaVersion, pluginabi.SchemaVersion)
	}
	for _, key := range []string{"scheduler", "scheduler_across_priorities", "request_interceptor", "request_lifecycle_plugin", "management_api"} {
		if !wire.Capabilities[key] {
			t.Fatalf("capability %q not declared: %v", key, wire.Capabilities)
		}
	}

	// The host decodes metadata into pluginapi.Metadata, which has no JSON tags.
	var metadata pluginapi.Metadata
	if err := json.Unmarshal(wire.Metadata, &metadata); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if metadata.Name != pluginName || metadata.Version != pluginVersion {
		t.Fatalf("metadata = %+v", metadata)
	}
	if len(metadata.ConfigFields) == 0 {
		t.Fatal("metadata declares no config fields")
	}
}

func TestPluginReconfigureKeepsInflightState(t *testing.T) {
	mustRegister(t, testConfigYAML)
	decodeOK(t, handleMethod(pluginabi.MethodSchedulerPick, pickJSON("local-llm-group", "local-llm-group")))
	decodeOK(t, handleMethod(pluginabi.MethodRequestInterceptAfter, interceptJSON("req-0", "auth-a")))
	if got := readStatus(t).Totals.Inflight; got != 1 {
		t.Fatalf("inflight before reconfigure = %d, want 1", got)
	}

	decodeOK(t, handleMethod(pluginabi.MethodPluginReconfigure, lifecycleJSON(t, `
group_aliases: ["local-llm-group"]
backends:
  - match: { compat_name: "gpu-box-a" }
    max_concurrency: 4
  - match: { compat_name: "gpu-box-b" }
    max_concurrency: 2
`, pluginabi.SchemaVersion)))

	status := readStatus(t)
	if status.Totals.Inflight != 1 {
		t.Fatalf("inflight after reconfigure = %d, want 1", status.Totals.Inflight)
	}
	if status.Backends[0].MaxConcurrency != 4 {
		t.Fatalf("gpu-box-a max after reconfigure = %d, want 4", status.Backends[0].MaxConcurrency)
	}
}

func TestPluginRegisterRejectsInvalidConfig(t *testing.T) {
	resetState()
	t.Cleanup(resetState)
	errEnvelope := decodeErr(t, handleMethod(pluginabi.MethodPluginRegister, lifecycleJSON(t, "group_aliases: []\n", pluginabi.SchemaVersion)))
	if errEnvelope.Code != "invalid_config" {
		t.Fatalf("error code = %q", errEnvelope.Code)
	}
	if currentTracker() != nil {
		t.Fatal("invalid configuration installed a tracker")
	}
}

func TestPluginRegisterRejectsOldHostSchema(t *testing.T) {
	resetState()
	t.Cleanup(resetState)
	errEnvelope := decodeErr(t, handleMethod(pluginabi.MethodPluginRegister, lifecycleJSON(t, testConfigYAML, 1)))
	if errEnvelope.Code != "unsupported_schema" {
		t.Fatalf("error code = %q", errEnvelope.Code)
	}
}

func TestSchedulerPickEnvelope(t *testing.T) {
	mustRegister(t, testConfigYAML)

	result := decodeOK(t, handleMethod(pluginabi.MethodSchedulerPick, pickJSON("local-llm-group", "local-llm-group")))
	var wire map[string]any
	if err := json.Unmarshal(result, &wire); err != nil {
		t.Fatalf("decode pick result: %v", err)
	}
	if wire["AuthID"] != "auth-a" || wire["Handled"] != true {
		t.Fatalf("pick result = %v", wire)
	}

	// The host decodes the same bytes into pluginapi.SchedulerPickResponse.
	var response pluginapi.SchedulerPickResponse
	if err := json.Unmarshal(result, &response); err != nil {
		t.Fatalf("decode into SDK type: %v", err)
	}
	if !response.Handled || response.AuthID != "auth-a" {
		t.Fatalf("pick response = %+v", response)
	}
}

func TestSchedulerPickUsesRequestedModelMetadata(t *testing.T) {
	mustRegister(t, testConfigYAML)

	result := decodeOK(t, handleMethod(pluginabi.MethodSchedulerPick, pickJSON("model-a", "local-llm-group")))
	var response pluginapi.SchedulerPickResponse
	if err := json.Unmarshal(result, &response); err != nil {
		t.Fatalf("decode pick result: %v", err)
	}
	if !response.Handled || response.AuthID != "auth-a" {
		t.Fatalf("pick response = %+v", response)
	}
}

func TestSchedulerPickLeavesNonGroupModelsUnhandled(t *testing.T) {
	mustRegister(t, testConfigYAML)

	result := decodeOK(t, handleMethod(pluginabi.MethodSchedulerPick, pickJSON("gpt-4o", "gpt-4o")))
	var response pluginapi.SchedulerPickResponse
	if err := json.Unmarshal(result, &response); err != nil {
		t.Fatalf("decode pick result: %v", err)
	}
	if response.Handled || response.AuthID != "" {
		t.Fatalf("pick response = %+v", response)
	}
}

func TestSchedulerPickRejectReturns429(t *testing.T) {
	mustRegister(t, `
group_aliases: ["local-llm-group"]
on_saturated: reject
backends:
  - match: { compat_name: "gpu-box-a" }
    max_concurrency: 1
  - match: { compat_name: "gpu-box-b" }
    max_concurrency: 1
`)
	decodeOK(t, handleMethod(pluginabi.MethodSchedulerPick, pickJSON("local-llm-group", "local-llm-group")))
	decodeOK(t, handleMethod(pluginabi.MethodSchedulerPick, pickJSON("local-llm-group", "local-llm-group")))

	errEnvelope := decodeErr(t, handleMethod(pluginabi.MethodSchedulerPick, pickJSON("local-llm-group", "local-llm-group")))
	if errEnvelope.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("http_status = %d, want 429", errEnvelope.HTTPStatus)
	}
	if errEnvelope.Code != "local_llm_backends_saturated" {
		t.Fatalf("error code = %q", errEnvelope.Code)
	}

	// http_status must be present under that exact key for the host to read it.
	var wire struct {
		Error struct {
			HTTPStatus int `json:"http_status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(handleMethod(pluginabi.MethodSchedulerPick, pickJSON("local-llm-group", "local-llm-group")), &wire); err != nil {
		t.Fatalf("decode raw error envelope: %v", err)
	}
	if wire.Error.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("raw http_status = %d", wire.Error.HTTPStatus)
	}
}

func TestRequestInterceptBeforePassesThrough(t *testing.T) {
	mustRegister(t, testConfigYAML)

	result := decodeOK(t, handleMethod(pluginabi.MethodRequestInterceptBefore, interceptJSON("req-0", "auth-a")))
	var response pluginapi.RequestInterceptResponse
	if err := json.Unmarshal(result, &response); err != nil {
		t.Fatalf("decode intercept response: %v", err)
	}
	if response.Terminate {
		t.Fatal("intercept_before terminated the request")
	}
	if got := response.Headers.Get("Content-Type"); got != "application/json" {
		t.Fatalf("headers = %v", response.Headers)
	}
	if string(response.Body) != `{"model":"local-llm-group"}` {
		t.Fatalf("body = %s", response.Body)
	}
	if got := readStatus(t).Totals.Inflight; got != 0 {
		t.Fatalf("intercept_before bound a slot: inflight = %d", got)
	}
}

func TestRequestInterceptAfterBindsSelectedAuth(t *testing.T) {
	mustRegister(t, testConfigYAML)
	decodeOK(t, handleMethod(pluginabi.MethodSchedulerPick, pickJSON("local-llm-group", "local-llm-group")))

	result := decodeOK(t, handleMethod(pluginabi.MethodRequestInterceptAfter, interceptJSON("req-0", "auth-a")))
	var response pluginapi.RequestInterceptResponse
	if err := json.Unmarshal(result, &response); err != nil {
		t.Fatalf("decode intercept response: %v", err)
	}
	if response.Terminate || string(response.Body) != `{"model":"local-llm-group"}` {
		t.Fatalf("intercept_after altered the request: %+v", response)
	}

	status := readStatus(t)
	if status.Backends[0].Inflight != 1 || status.Backends[0].Pending != 0 {
		t.Fatalf("gpu-box-a status = %+v", status.Backends[0])
	}

	// A retry onto the other credential moves the slot.
	decodeOK(t, handleMethod(pluginabi.MethodRequestInterceptAfter, interceptJSON("req-0", "auth-b")))
	status = readStatus(t)
	if status.Backends[0].Inflight != 0 || status.Backends[1].Inflight != 1 {
		t.Fatalf("status after retry = %+v", status.Backends)
	}
}

func TestRequestCompleteReleasesSlot(t *testing.T) {
	mustRegister(t, testConfigYAML)
	decodeOK(t, handleMethod(pluginabi.MethodSchedulerPick, pickJSON("local-llm-group", "local-llm-group")))
	decodeOK(t, handleMethod(pluginabi.MethodRequestInterceptAfter, interceptJSON("req-0", "auth-a")))

	decodeOK(t, handleMethod(pluginabi.MethodRequestComplete, completionJSON("req-0")))
	decodeOK(t, handleMethod(pluginabi.MethodRequestComplete, completionJSON("req-0")))
	decodeOK(t, handleMethod(pluginabi.MethodRequestComplete, completionJSON("never-seen")))

	status := readStatus(t)
	if status.Totals.Inflight != 0 || status.Totals.TrackedRequests != 0 {
		t.Fatalf("totals after completion = %+v", status.Totals)
	}
	if status.Backends[0].Available != 8 {
		t.Fatalf("gpu-box-a available = %d, want 8", status.Backends[0].Available)
	}
}

func TestManagementRegisterEnvelope(t *testing.T) {
	mustRegister(t, testConfigYAML)

	result := decodeOK(t, handleMethod(pluginabi.MethodManagementRegister, []byte(`{"Plugin":{"Name":"local-llm-pool"},"BasePath":"/v0/management","ResourceBasePath":"/v0/resource/plugins/local-llm-pool"}`)))

	var wire struct {
		Resources []map[string]any `json:"resources"`
	}
	if err := json.Unmarshal(result, &wire); err != nil {
		t.Fatalf("decode management registration: %v (%s)", err, result)
	}
	if len(wire.Resources) != 1 || wire.Resources[0]["Path"] != statusResourcePath {
		t.Fatalf("resources = %v", wire.Resources)
	}
	if wire.Resources[0]["Menu"] == "" || wire.Resources[0]["Description"] == "" {
		t.Fatalf("resource is missing menu metadata: %v", wire.Resources[0])
	}
}

func TestManagementHandleStatus(t *testing.T) {
	mustRegister(t, testConfigYAML)
	decodeOK(t, handleMethod(pluginabi.MethodSchedulerPick, pickJSON("local-llm-group", "local-llm-group")))
	decodeOK(t, handleMethod(pluginabi.MethodRequestInterceptAfter, interceptJSON("req-0", "auth-a")))

	status := readStatus(t)
	if len(status.Backends) != 2 {
		t.Fatalf("backends = %+v", status.Backends)
	}
	if status.Backends[0].Name != "gpu-box-a" || status.Backends[0].MaxConcurrency != 8 || status.Backends[0].Inflight != 1 || status.Backends[0].Available != 7 {
		t.Fatalf("gpu-box-a = %+v", status.Backends[0])
	}
	if status.Backends[1].Name != "gpu-box-b" || status.Backends[1].MaxConcurrency != 2 {
		t.Fatalf("gpu-box-b = %+v", status.Backends[1])
	}
	if status.Totals.MaxConcurrency != 10 || status.Totals.Inflight != 1 || status.Totals.Available != 9 || status.Totals.TrackedRequests != 1 {
		t.Fatalf("totals = %+v", status.Totals)
	}
	if status.Config.OnSaturated != string(tracker.PolicyLeastLoaded) || status.Config.PendingTTLMS != 5000 {
		t.Fatalf("config = %+v", status.Config)
	}
	if len(status.Config.GroupAliases) != 1 || status.Config.GroupAliases[0] != "local-llm-group" {
		t.Fatalf("config aliases = %v", status.Config.GroupAliases)
	}
}

func TestManagementHandleRejectsOtherRoutes(t *testing.T) {
	mustRegister(t, testConfigYAML)

	for _, call := range []struct{ method, path string }{
		{http.MethodPost, "/v0/resource/plugins/local-llm-pool/status"},
		{http.MethodGet, "/v0/resource/plugins/local-llm-pool/other"},
	} {
		result := decodeOK(t, handleMethod(pluginabi.MethodManagementHandle, managementJSON(call.method, call.path)))
		var response pluginapi.ManagementResponse
		if err := json.Unmarshal(result, &response); err != nil {
			t.Fatalf("decode management response: %v", err)
		}
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("%s %s returned %d, want 404", call.method, call.path, response.StatusCode)
		}
	}
}

func TestManagementHandleBeforeConfigure(t *testing.T) {
	resetState()
	t.Cleanup(resetState)

	result := decodeOK(t, handleMethod(pluginabi.MethodManagementHandle, managementJSON(http.MethodGet, "/v0/resource/plugins/local-llm-pool/status")))
	var response pluginapi.ManagementResponse
	if err := json.Unmarshal(result, &response); err != nil {
		t.Fatalf("decode management response: %v", err)
	}
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status before configure = %d, want 503", response.StatusCode)
	}
}

func TestLifecycleNoOpMethods(t *testing.T) {
	mustRegister(t, testConfigYAML)
	decodeOK(t, handleMethod(pluginabi.MethodPluginQuiesce, []byte(`{}`)))
	if currentTracker() == nil {
		t.Fatal("quiesce dropped the tracker")
	}
	decodeOK(t, handleMethod(pluginabi.MethodPluginShutdown, []byte(`{}`)))
	if currentTracker() != nil {
		t.Fatal("shutdown kept the tracker")
	}
}

func TestUnknownMethod(t *testing.T) {
	errEnvelope := decodeErr(t, handleMethod("does.not.exist", nil))
	if errEnvelope.Code != "unknown_method" {
		t.Fatalf("error code = %q", errEnvelope.Code)
	}
}

func TestMalformedRequestPayloads(t *testing.T) {
	mustRegister(t, testConfigYAML)
	for _, method := range []string{
		pluginabi.MethodSchedulerPick,
		pluginabi.MethodRequestInterceptBefore,
		pluginabi.MethodRequestInterceptAfter,
		pluginabi.MethodRequestComplete,
		pluginabi.MethodManagementHandle,
	} {
		if errEnvelope := decodeErr(t, handleMethod(method, []byte(`{`))); errEnvelope.Code != "invalid_request" {
			t.Fatalf("%s error code = %q", method, errEnvelope.Code)
		}
	}
}

// TestRequestInterceptAfterForwardsRequestedModel proves the dispatch layer
// hands RequestedModel to the tracker. The two calls below differ only in that
// field, and only the group one may consume the outstanding reservation.
func TestRequestInterceptAfterForwardsRequestedModel(t *testing.T) {
	mustRegister(t, testConfigYAML)

	decodeOK(t, handleMethod(pluginabi.MethodSchedulerPick, pickJSON("local-llm-group", "local-llm-group")))
	if boxA := readStatus(t).Backends[0]; boxA.Pending != 1 || boxA.Inflight != 0 {
		t.Fatalf("gpu-box-a status after pick = %+v", boxA)
	}

	decodeOK(t, handleMethod(pluginabi.MethodRequestInterceptAfter, interceptJSONFor("direct-0", "auth-a", "model-a", "gpu-box-a")))
	boxA := readStatus(t).Backends[0]
	if boxA.Inflight != 1 || boxA.Pending != 1 {
		t.Fatalf("gpu-box-a status after direct-alias bind = %+v", boxA)
	}

	decodeOK(t, handleMethod(pluginabi.MethodRequestInterceptAfter, interceptJSONFor("group-0", "auth-a", "model-a", "local-llm-group")))
	boxA = readStatus(t).Backends[0]
	if boxA.Inflight != 2 || boxA.Pending != 0 {
		t.Fatalf("gpu-box-a status after group bind = %+v", boxA)
	}
}

// poolsConfigYAML exercises the multi-pool form across the RPC boundary, with
// one backend shared by both pools.
const poolsConfigYAML = `enabled: true
priority: 1
on_saturated: least-loaded
pending_ttl_ms: 5000
pools:
  - group_aliases: ["local-llm-group"]
    backends:
      - match: { compat_name: "gpu-box-a" }
        max_concurrency: 1
      - match: { compat_name: "gpu-box-b" }
        max_concurrency: 1
  - group_aliases: ["spill-group"]
    on_saturated: reject
    backends:
      - match: { compat_name: "gpu-box-b" }
        max_concurrency: 1
`

func TestPluginRegisterAcceptsPoolsConfig(t *testing.T) {
	resetState()
	t.Cleanup(resetState)
	result := decodeOK(t, handleMethod(pluginabi.MethodPluginRegister, lifecycleJSON(t, poolsConfigYAML, pluginabi.SchemaVersion)))

	var wire struct {
		Metadata json.RawMessage `json:"metadata"`
	}
	if err := json.Unmarshal(result, &wire); err != nil {
		t.Fatalf("decode registration: %v", err)
	}
	var metadata pluginapi.Metadata
	if err := json.Unmarshal(wire.Metadata, &metadata); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	declared := map[string]bool{}
	for _, field := range metadata.ConfigFields {
		declared[field.Name] = true
	}
	for _, name := range []string{"group_aliases", "on_saturated", "pending_ttl_ms", "backends", "pools"} {
		if !declared[name] {
			t.Fatalf("config field %q not declared: %v", name, declared)
		}
	}
}

func TestPluginRegisterRejectsBothConfigForms(t *testing.T) {
	resetState()
	t.Cleanup(resetState)
	both := testConfigYAML + `pools:
  - group_aliases: ["other-group"]
    backends:
      - match: { compat_name: "gpu-box-a" }
        max_concurrency: 1
`
	if errEnvelope := decodeErr(t, handleMethod(pluginabi.MethodPluginRegister, lifecycleJSON(t, both, pluginabi.SchemaVersion))); errEnvelope.Code != "invalid_config" {
		t.Fatalf("error code = %q", errEnvelope.Code)
	}
	if currentTracker() != nil {
		t.Fatal("invalid configuration installed a tracker")
	}
}

func TestSchedulerPickRoutesPerPoolOverRPC(t *testing.T) {
	mustRegister(t, poolsConfigYAML)

	// The first pool fills gpu-box-a, then spills to the shared gpu-box-b backend.
	first := pickResponse(t, "local-llm-group", "local-llm-group")
	if !first.Handled || first.AuthID != "auth-a" {
		t.Fatalf("first pick = %+v", first)
	}
	decodeOK(t, handleMethod(pluginabi.MethodRequestInterceptAfter, interceptJSONFor("req-0", "auth-a", "model-a", "local-llm-group")))

	second := pickResponse(t, "local-llm-group", "local-llm-group")
	if !second.Handled || second.AuthID != "auth-b" {
		t.Fatalf("second pick = %+v", second)
	}
	decodeOK(t, handleMethod(pluginabi.MethodRequestInterceptAfter, interceptJSONFor("req-1", "auth-b", "model-a", "local-llm-group")))

	// gpu-box-b is now full, and the second pool sees the same counter, so its
	// reject policy fires.
	errEnvelope := decodeErr(t, handleMethod(pluginabi.MethodSchedulerPick, pickJSON("spill-group", "spill-group")))
	if errEnvelope.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("http_status = %d, want 429", errEnvelope.HTTPStatus)
	}
}

func TestSchedulerPickMatchesReservedProviderKeyOverRPC(t *testing.T) {
	mustRegister(t, `
group_aliases: ["local-llm-group"]
backends:
  - match: { provider: "openai-compatibility", compat_name: "gpu-box-b" }
    max_concurrency: 1
`)
	// pickJSON sets Provider "openai-compatibility" on both candidates, so only
	// the compat_name narrows the match down to gpu-box-b.
	response := pickResponse(t, "local-llm-group", "local-llm-group")
	if !response.Handled || response.AuthID != "auth-b" {
		t.Fatalf("pick = %+v", response)
	}
}

func TestManagementHandleStatusReportsPools(t *testing.T) {
	mustRegister(t, poolsConfigYAML)
	decodeOK(t, handleMethod(pluginabi.MethodSchedulerPick, pickJSON("local-llm-group", "local-llm-group")))
	decodeOK(t, handleMethod(pluginabi.MethodRequestInterceptAfter, interceptJSONFor("req-0", "auth-a", "model-a", "local-llm-group")))

	status := readStatus(t)
	if len(status.Pools) != 2 {
		t.Fatalf("pools = %+v", status.Pools)
	}
	if got := status.Pools[0]; got.Name != "local-llm-group" || got.OnSaturated != string(tracker.PolicyLeastLoaded) ||
		len(got.Backends) != 2 || got.Backends[0] != "gpu-box-a" || got.Backends[1] != "gpu-box-b" {
		t.Fatalf("pool 0 = %+v", got)
	}
	if got := status.Pools[1]; got.Name != "spill-group" || got.OnSaturated != string(tracker.PolicyReject) ||
		len(got.Backends) != 1 || got.Backends[0] != "gpu-box-b" {
		t.Fatalf("pool 1 = %+v", got)
	}

	// gpu-box-b is shared, so it appears once in the backend list.
	if len(status.Backends) != 2 {
		t.Fatalf("backends = %+v", status.Backends)
	}

	// Legacy keys survive for consumers written before pools existed.
	if status.Totals.MaxConcurrency != 2 || status.Totals.Inflight != 1 || status.Totals.TrackedRequests != 1 {
		t.Fatalf("totals = %+v", status.Totals)
	}
	if got := status.Config.GroupAliases; len(got) != 2 || got[0] != "local-llm-group" || got[1] != "spill-group" {
		t.Fatalf("config aliases = %v", got)
	}
	if status.Config.OnSaturated != string(tracker.PolicyLeastLoaded) || status.Config.PendingTTLMS != 5000 {
		t.Fatalf("config = %+v", status.Config)
	}
}

// TestManagementHandleStatusRawJSONKeys pins the wire key names so a consumer
// reading the endpoint directly is not broken by a Go field rename.
func TestManagementHandleStatusRawJSONKeys(t *testing.T) {
	mustRegister(t, testConfigYAML)

	result := decodeOK(t, handleMethod(pluginabi.MethodManagementHandle, managementJSON(http.MethodGet, "/v0/resource/plugins/local-llm-pool/status")))
	var response pluginapi.ManagementResponse
	if err := json.Unmarshal(result, &response); err != nil {
		t.Fatalf("decode management response: %v", err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(response.Body, &body); err != nil {
		t.Fatalf("decode status body: %v", err)
	}
	for _, key := range []string{"pools", "backends", "totals", "config"} {
		if _, ok := body[key]; !ok {
			t.Fatalf("status body is missing key %q: %s", key, response.Body)
		}
	}

	var pools []map[string]json.RawMessage
	if err := json.Unmarshal(body["pools"], &pools); err != nil {
		t.Fatalf("decode pools: %v", err)
	}
	for _, key := range []string{"name", "group_aliases", "on_saturated", "backends"} {
		if _, ok := pools[0][key]; !ok {
			t.Fatalf("pool entry is missing key %q", key)
		}
	}

	var backends []map[string]json.RawMessage
	if err := json.Unmarshal(body["backends"], &backends); err != nil {
		t.Fatalf("decode backends: %v", err)
	}
	for _, key := range []string{"name", "match", "max_concurrency", "inflight", "pending", "available"} {
		if _, ok := backends[0][key]; !ok {
			t.Fatalf("backend entry is missing key %q", key)
		}
	}

	var totals map[string]json.RawMessage
	if err := json.Unmarshal(body["totals"], &totals); err != nil {
		t.Fatalf("decode totals: %v", err)
	}
	for _, key := range []string{"max_concurrency", "inflight", "pending", "available", "tracked_requests"} {
		if _, ok := totals[key]; !ok {
			t.Fatalf("totals is missing key %q", key)
		}
	}
}
