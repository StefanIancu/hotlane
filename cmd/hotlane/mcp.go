// hotlane mcp: a Model Context Protocol server over stdio, so MCP-capable
// agents get hotlane as typed tools - no shell, no output parsing, no docs
// required. Thin adapter over the daemon HTTP API; run it from the app's
// repo (push/test compute the git diff from the working directory).
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func objSchema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

var mcpTools = []mcpTool{
	{"hotlane_status", "Deployment state: live version, ring of rollback targets, held test forks, archive/drift verdict, baseline commit.", objSchema(map[string]any{})},
	{"hotlane_push", "Deploy the working directory's changes: fork the live app, apply the git delta, verify in isolation, atomically promote. Returns JSON with promoted true/false, phase timings, per-hook verdicts, and the fork's logs on rejection. A rejected push never receives traffic.", objSchema(map[string]any{
		"from": map[string]any{"type": "string", "description": "git ref to diff from (default: the daemon's baseline commit)"},
	})},
	{"hotlane_test", "Like push, but HOLD the verified fork instead of promoting: users stay on live while you validate the fork by sending requests to the app URL with the returned X-Hotlane-Fork header. Then hotlane_promote or hotlane_discard. Held forks expire after the returned TTL.", objSchema(map[string]any{
		"from": map[string]any{"type": "string", "description": "git ref to diff from (default: the daemon's baseline commit)"},
	})},
	{"hotlane_promote", "Flip traffic to a held fork - byte-identical to what you tested. Verify hooks re-run as a gate; failure aborts with live untouched.", objSchema(map[string]any{
		"version": map[string]any{"type": "integer", "description": "held fork version from hotlane_test"},
	}, "version")},
	{"hotlane_discard", "Destroy a held fork; live traffic never knew it existed.", objSchema(map[string]any{
		"version": map[string]any{"type": "integer", "description": "held fork version from hotlane_test"},
	}, "version")},
	{"hotlane_rollback", "Flip traffic to a previous kept version (sub-second, no builds). Omit version for the one before live.", objSchema(map[string]any{
		"version": map[string]any{"type": "integer", "description": "specific kept version (optional)"},
	})},
	{"hotlane_drift", "Cold-boot the archivist's from-source clean image and diff its behavior against live. Returns the drift verdict; drifted means live no longer matches the source of record.", objSchema(map[string]any{})},
	{"hotlane_logs", "Tail the live version's stdout/stderr.", objSchema(map[string]any{
		"tail": map[string]any{"type": "integer", "description": "number of lines (default 100)"},
	})},
}

func cmdMCP(args []string) {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	daemon := fs.String("daemon", daemonDefault(), "daemon API base URL")
	appName := fs.String("app", "", "app name on a multi-app daemon (default: HOTLANE_APP, else the app named by ./hotlane.yml)")
	fs.Parse(args)

	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 1<<20), 16<<20)
	out := bufio.NewWriter(os.Stdout)

	reply := func(id json.RawMessage, result any, rpcErr map[string]any) {
		if id == nil {
			return // notification: no response
		}
		msg := map[string]any{"jsonrpc": "2.0", "id": id}
		if rpcErr != nil {
			msg["error"] = rpcErr
		} else {
			msg["result"] = result
		}
		b, _ := json.Marshal(msg)
		out.Write(b)
		out.WriteByte('\n')
		out.Flush()
	}

	for in.Scan() {
		line := bytes.TrimSpace(in.Bytes())
		if len(line) == 0 {
			continue
		}
		// A batch (JSON array) is valid JSON-RPC 2.0 and part of the
		// protocol version advertised below, and malformed JSON demands
		// -32700. Silently `continue`ing on either left the client
		// blocked forever on a request id that would never be answered.
		if line[0] == '[' {
			reply(json.RawMessage("null"), nil, map[string]any{
				"code": -32600, "message": "batch requests are not supported; send one request per line",
			})
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			reply(json.RawMessage("null"), nil, map[string]any{
				"code": -32700, "message": "parse error: " + err.Error(),
			})
			continue
		}
		switch req.Method {
		case "initialize":
			reply(req.ID, map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "hotlane", "version": version},
			}, nil)
		case "notifications/initialized", "notifications/cancelled":
			// no response
		case "ping":
			reply(req.ID, map[string]any{}, nil)
		case "tools/list":
			reply(req.ID, map[string]any{"tools": mcpTools}, nil)
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			json.Unmarshal(req.Params, &p)
			text, isErr := mcpCall(*daemon, clientBase(*appName), p.Name, p.Arguments)
			reply(req.ID, map[string]any{
				"content": []map[string]any{{"type": "text", "text": text}},
				"isError": isErr,
			}, nil)
		default:
			reply(req.ID, nil, map[string]any{"code": -32601, "message": "method not found: " + req.Method})
		}
	}
	// A line over the scanner's buffer (16MB) ends Scan with an error
	// that used to go unchecked: the loop exited and the process
	// returned 0, so the agent saw a clean shutdown indistinguishable
	// from a normal close, mid-session.
	if err := in.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "hotlane mcp: input stream failed: %v\n", err)
		os.Exit(1)
	}
}

// mcpCall dispatches one tool invocation against the daemon API and
// returns the response body (JSON text) and whether it is an error.
func mcpCall(daemon, base, name string, args map[string]any) (string, bool) {
	// argInt must distinguish "absent" from "zero". For rollback,
	// version 0 is the legitimate encoding of "the version before
	// live", so a client sending {"version":"3"} - a routine LLM
	// coercion - would silently roll production back one version and
	// report success. Strings that look like numbers are accepted;
	// anything else is an explicit error.
	argInt := func(k string) (int, bool) {
		switch v := args[k].(type) {
		case float64:
			return int(v), true
		case string:
			n, err := strconv.Atoi(strings.TrimSpace(v))
			return n, err == nil
		}
		return 0, false
	}
	needInt := func(k string) (int, string) {
		n, ok := argInt(k)
		if !ok {
			return 0, fmt.Sprintf(`{"error":"%s is required and must be a number"}`, k)
		}
		return n, ""
	}
	argStr := func(k string) string {
		v, _ := args[k].(string)
		return v
	}
	do := func(method, path string, body []byte, contentType string) (string, bool) {
		resp, err := appRequest(method, daemon, base, path, contentType, body)
		if err != nil {
			return fmt.Sprintf(`{"error":%q}`, err.Error()), true
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		txt := string(bytes.TrimSpace(b))
		if txt == "" {
			txt = fmt.Sprintf(`{"status":%d}`, resp.StatusCode)
		}
		return txt, resp.StatusCode >= 400
	}
	jsonBody := func(v any) []byte {
		b, _ := json.Marshal(v)
		return b
	}

	switch name {
	case "hotlane_status":
		return do("GET", "/status", nil, "")
	case "hotlane_push", "hotlane_test":
		diff, err := computeDiffE(daemon, base, argStr("from"))
		if err != nil {
			return fmt.Sprintf(`{"error":%q}`, err.Error()), true
		}
		path := "/push"
		if name == "hotlane_test" {
			path = "/test"
		}
		return do("POST", path, diff, "text/x-diff")
	case "hotlane_promote":
		v, errMsg := needInt("version")
		if errMsg != "" {
			return errMsg, true
		}
		return do("POST", "/promote", jsonBody(map[string]int{"version": v}), "application/json")
	case "hotlane_discard":
		v, errMsg := needInt("version")
		if errMsg != "" {
			return errMsg, true
		}
		return do("POST", "/discard", jsonBody(map[string]int{"version": v}), "application/json")
	case "hotlane_rollback":
		// version is genuinely optional here, but an UNPARSEABLE one
		// must not silently become "the version before live".
		v := 0
		if raw, present := args["version"]; present && raw != nil {
			n, ok := argInt("version")
			if !ok {
				return `{"error":"version must be a number (omit it to roll back to the version before live)"}`, true
			}
			v = n
		}
		return do("POST", "/rollback", jsonBody(map[string]int{"version": v}), "application/json")
	case "hotlane_drift":
		return do("POST", "/drift-check", nil, "application/json")
	case "hotlane_logs":
		tail, _ := argInt("tail")
		if tail <= 0 {
			tail = 100
		}
		return do("GET", fmt.Sprintf("/logs?tail=%d", tail), nil, "")
	default:
		return fmt.Sprintf(`{"error":"unknown tool %q"}`, name), true
	}
}
