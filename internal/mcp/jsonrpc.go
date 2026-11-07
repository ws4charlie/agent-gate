// Package mcp holds the protocol pieces of the MCP tool gateway: JSON-RPC
// framing, the Streamable HTTP request checks, SSE event reading, the config
// of upstream servers, and an in-repo fake server for tests and the offline
// demo. The gate itself (auth, policy, ledger, screening, audit) is in
// internal/server, because it reuses that package's request machinery.
//
// Protocol version targeted: 2026-07-28 (modelcontextprotocol.io, read
// 2026-11-03). That revision has no sessions and no `initialize` handshake:
// every POST carries its protocol version and client capabilities in
// params._meta, mirrored into the MCP-Protocol-Version, Mcp-Method and
// Mcp-Name headers. So the gateway is stateless too: it keeps no per-client
// MCP state, ignores Mcp-Session-Id, and answers GET and DELETE with 405.
//
// Many real clients still speak the handshake revisions (2025-11-25 and
// earlier). The gateway adapts them at its edge and stays stateless: it answers
// `initialize` itself, without a session, and upgrades every later message to
// 2026-07-28 before checking and forwarding it. Upstreams only ever see the
// modern revision.
package mcp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// ProtocolVersion is the one revision the gateway speaks.
const ProtocolVersion = "2026-07-28"

// LegacyVersions are the handshake revisions adapted at the gateway's edge,
// oldest first.
var LegacyVersions = []string{"2025-03-26", "2025-06-18", "2025-11-25"}

// Supported lists the versions named in UnsupportedProtocolVersion errors.
var Supported = append(append([]string{}, LegacyVersions...), ProtocolVersion)

// JSON-RPC and MCP error codes. -32020..-32022 are defined by the 2026-07-28
// spec; the spec reserves -32020..-32099 for itself, so the gateway's own
// refusals use codes outside the JSON-RPC reserved range, as the spec asks.
const (
	CodeParse          = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603
	CodeHeaderMismatch = -32020
	CodeUnsupportedVer = -32022
	CodeInsufficient   = 40201 // not enough funds for the tool's price
	CodeApproval       = 40202 // a human must approve; data.approval_id
	CodePolicyDenied   = 40301 // scope, budget, velocity, killed...; data.rule
	CodeScreened       = 40302 // the tool result was blocked by the screen; data.rule
	CodeDuplicate      = 40901 // Idempotency-Key already used
	CodeUpstream       = 50201 // the upstream MCP server failed
)

// Request is a JSON-RPC request or notification (ID absent).
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// IsNotification reports a message without an id.
func (r Request) IsNotification() bool { return len(r.ID) == 0 }

// Error is the JSON-RPC error object.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("mcp %d: %s", e.Code, e.Message) }

// ErrorResponse renders a JSON-RPC error response. A missing id becomes null,
// which JSON-RPC allows exactly when the id could not be read.
func ErrorResponse(id json.RawMessage, e Error) []byte {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	b, _ := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   Error           `json:"error"`
	}{"2.0", id, e})
	return b
}

// Parse decodes one JSON-RPC message. A batch (array) is refused: the
// 2026-07-28 transport sends one request or notification per POST.
func Parse(body []byte) (Request, *Error) {
	body = bytes.TrimSpace(body)
	if len(body) > 0 && body[0] == '[' {
		return Request{}, &Error{Code: CodeInvalidRequest, Message: "batches are not part of Streamable HTTP in 2026-07-28: one message per POST"}
	}
	var r Request
	if err := json.Unmarshal(body, &r); err != nil {
		return Request{}, &Error{Code: CodeParse, Message: "body is not valid JSON"}
	}
	if r.JSONRPC != "2.0" || r.Method == "" {
		return r, &Error{Code: CodeInvalidRequest, Message: "need jsonrpc 2.0 and a method"}
	}
	if len(r.ID) > 0 && string(r.ID) == "null" {
		return r, &Error{Code: CodeInvalidRequest, Message: "id must not be null"}
	}
	return r, nil
}

// Meta keys of the per-request protocol fields.
const (
	metaVersion = "io.modelcontextprotocol/protocolVersion"
	metaCaps    = "io.modelcontextprotocol/clientCapabilities"
)

// Check applies the Streamable HTTP request rules of 2026-07-28 to a parsed
// request and returns the HTTP status and error to answer with, or nil.
//
//   - the version must be in _meta and in MCP-Protocol-Version, equal (400, -32020 / -32602)
//   - an unsupported version gets UnsupportedProtocolVersion with the list (400, -32022)
//   - Mcp-Method must equal the body's method; Mcp-Name must equal params.name
//     (tools/call, prompts/get) or params.uri (resources/read), after decoding
//     the =?base64?...?= form (400, -32020)
//
// Why check headers at all in a proxy: the spec says any server that processes
// the body must, because a load balancer may route on the header while the
// body says something else. The gateway decides policy from the body, so a
// header that disagrees with it is rejected, not trusted.
func Check(h http.Header, r Request) (int, *Error) {
	hv := h.Get("MCP-Protocol-Version")
	if hv == "" {
		return http.StatusBadRequest, &Error{Code: CodeHeaderMismatch, Message: "missing MCP-Protocol-Version header"}
	}
	var p struct {
		Name string                     `json:"name"`
		URI  string                     `json:"uri"`
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	_ = json.Unmarshal(r.Params, &p)
	var mv string
	if raw, ok := p.Meta[metaVersion]; ok {
		_ = json.Unmarshal(raw, &mv)
	}
	if mv == "" || len(p.Meta[metaCaps]) == 0 {
		return http.StatusBadRequest, &Error{Code: CodeInvalidParams, Message: "params._meta needs " + metaVersion + " and " + metaCaps}
	}
	if mv != hv {
		return http.StatusBadRequest, &Error{Code: CodeHeaderMismatch, Message: "MCP-Protocol-Version does not match params._meta"}
	}
	if hv != ProtocolVersion {
		return http.StatusBadRequest, &Error{Code: CodeUnsupportedVer, Message: "unsupported protocol version",
			Data: map[string]any{"supported": Supported, "requested": hv}}
	}
	if h.Get("Mcp-Method") != r.Method {
		return http.StatusBadRequest, &Error{Code: CodeHeaderMismatch, Message: "Mcp-Method does not match the request method"}
	}
	want := ""
	switch r.Method {
	case "tools/call", "prompts/get":
		want = p.Name
	case "resources/read":
		want = p.URI
	default:
		return 0, nil
	}
	got, ok := DecodeHeaderValue(h.Get("Mcp-Name"))
	if !ok || got != want {
		return http.StatusBadRequest, &Error{Code: CodeHeaderMismatch, Message: "Mcp-Name does not match the request"}
	}
	return 0, nil
}

// Legacy reports a message from a handshake-era client: it has no version in
// params._meta, and its version header is absent or names a legacy revision.
func Legacy(h http.Header, r Request) bool {
	var p struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	_ = json.Unmarshal(r.Params, &p)
	if _, ok := p.Meta[metaVersion]; ok {
		return false
	}
	hv := h.Get("MCP-Protocol-Version")
	return hv == "" || slices.Contains(LegacyVersions, hv)
}

// InitializeResponse answers a legacy `initialize` at the gateway. It offers
// the client's version when the gateway knows it, else the newest legacy one,
// and no session: every later message stands on its own.
func InitializeResponse(r Request) []byte {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(r.Params, &p)
	version := LegacyVersions[len(LegacyVersions)-1]
	if slices.Contains(LegacyVersions, p.ProtocolVersion) {
		version = p.ProtocolVersion
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": r.ID, "result": map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]string{"name": "agentgate", "version": "0.1.0"},
	}})
	return b
}

// Upgrade rewrites a legacy message as 2026-07-28: it adds the _meta fields to
// the body and sets the mirrored headers from that body, so everything after
// it, policy included, sees one protocol. It reports the new body.
func Upgrade(h http.Header, r Request, body []byte) ([]byte, error) {
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, err
	}
	params := map[string]json.RawMessage{}
	if len(r.Params) > 0 && string(r.Params) != "null" {
		if err := json.Unmarshal(r.Params, &params); err != nil {
			return nil, err
		}
	}
	meta := map[string]json.RawMessage{}
	if raw, ok := params["_meta"]; ok {
		_ = json.Unmarshal(raw, &meta)
	}
	meta[metaVersion], _ = json.Marshal(ProtocolVersion)
	if _, ok := meta[metaCaps]; !ok {
		meta[metaCaps] = json.RawMessage("{}")
	}
	params["_meta"], _ = json.Marshal(meta)
	msg["params"], _ = json.Marshal(params)
	h.Set("MCP-Protocol-Version", ProtocolVersion)
	h.Set("Mcp-Method", r.Method)
	h.Del("Mcp-Name")
	for key, methods := range map[string][]string{"name": {"tools/call", "prompts/get"}, "uri": {"resources/read"}} {
		if !slices.Contains(methods, r.Method) {
			continue
		}
		var v string
		if err := json.Unmarshal(params[key], &v); err != nil {
			return nil, err
		}
		h.Set("Mcp-Name", EncodeHeaderValue(v))
	}
	return json.Marshal(msg)
}

// DecodeHeaderValue undoes the =?base64?...?= form used for header values that
// are not plain ASCII.
func DecodeHeaderValue(v string) (string, bool) {
	if strings.HasPrefix(v, "=?base64?") && strings.HasSuffix(v, "?=") && len(v) >= 11 {
		b, err := base64.StdEncoding.DecodeString(v[len("=?base64?") : len(v)-2])
		return string(b), err == nil
	}
	return v, v != ""
}

// EncodeHeaderValue is the client-side counterpart, used by the fake client in tests.
func EncodeHeaderValue(v string) string {
	plain := v != "" && !(strings.HasPrefix(v, "=?base64?") && strings.HasSuffix(v, "?=")) && v == strings.TrimSpace(v)
	for _, r := range v {
		if r < 0x20 || r > 0x7e {
			plain = false
		}
	}
	if plain {
		return v
	}
	return "=?base64?" + base64.StdEncoding.EncodeToString([]byte(v)) + "?="
}

// CallParams are the fields of tools/call the gateway reads.
type CallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}
