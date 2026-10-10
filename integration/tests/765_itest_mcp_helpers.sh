#!/usr/bin/env bash
# itest: no-docker
# Description: the itest MCP helpers — itest_json_field reads fields out of JSON, and itest_mcp_call does the streamable-HTTP handshake (bearer token, session id, initialized, tools/call, close) against a stand-in server, reads SSE and plain replies, waits for a late listener and does not retry a JSON-RPC error.
set -euo pipefail

source "$(dirname "$0")/../common.sh"

SERVER_PID=""
itest_cleanup() {
  if [ -n "${SERVER_PID}" ]; then
    kill "${SERVER_PID}" 2>/dev/null || true
    wait "${SERVER_PID}" 2>/dev/null || true
  fi
}
itest_begin

# Harness-only: no instance, so no setup_test. Everything lives under the
# scratch dir, which teardown removes.
work="${TEST_SCRATCH_DIR}/mcp-helpers"
mkdir -p "${work}"

# ===========================================
# itest_json_field
# ===========================================
info "itest_json_field"
doc='{"s":"héllo\nline2","n":7,"t":true,"f":false,"z":null,"a":[{"k":"v0"},{"k":"v1"}],"o":{"b":2,"a":1}}'
field() { printf '%s' "${doc}" | itest_json_field "$1"; }
assert_equals "$(printf 'héllo\nline2')" "$(field s)" "a string field, raw and UTF-8"
assert_equals "7" "$(field n)" "a number"
assert_equals "true" "$(field t)" "true"
assert_equals "false" "$(field f)" "false"
assert_equals "" "$(field z)" "null prints nothing"
assert_equals "v1" "$(field a.1.k)" "an array index"
assert_equals '{"a":1,"b":2}' "$(field o)" "an object, as JSON with sorted keys"
assert_equals '[{"k":"v0"},{"k":"v1"}]' "$(field a)" "an array, as JSON"
assert_equals "" "$(field a.5.k)" "an index past the end"
assert_equals "" "$(field s.x)" "a path through a string"
assert_equals "" "$(field missing.deeper)" "a missing key"
if printf 'not json' | itest_json_field s >/dev/null 2>&1; then
  fail "itest_json_field should fail on invalid JSON"
fi

# ===========================================
# _itest_mcp_result: the id-2 reply out of an SSE stream or a plain body.
# ===========================================
info "_itest_mcp_result"
sse=$(printf 'event: message\ndata: %s\n\nevent: message\ndata: %s\n\n' \
  '{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info"}}' \
  '{"jsonrpc":"2.0","id":2,"result":{"structuredContent":{"x":"sse"},"isError":false}}')
assert_equals '{"isError":false,"structuredContent":{"x":"sse"}}' \
  "$(printf '%s' "${sse}" | _itest_mcp_result)" "the reply in an SSE stream, past a notification"
assert_equals '{"content":[]}' \
  "$(printf '%s' '{"jsonrpc":"2.0","id":2,"result":{"content":[]}}' | _itest_mcp_result)" "a plain JSON reply"
rc=0
printf '%s' '{"jsonrpc":"2.0","id":2,"error":{"code":-32602,"message":"bad"}}' | _itest_mcp_result >/dev/null || rc=$?
assert_equals "2" "${rc}" "a JSON-RPC error"
rc=0
printf '%s' '{"jsonrpc":"2.0","id":1,"result":{}}' | _itest_mcp_result >/dev/null || rc=$?
assert_equals "1" "${rc}" "a reply to another request"
rc=0
printf '' | _itest_mcp_result >/dev/null || rc=$?
assert_equals "1" "${rc}" "no reply at all"

# ===========================================
# itest_mcp_call against a stand-in streamable-HTTP server.
# ===========================================
if ! command -v python3 >/dev/null 2>&1; then
  pass "itest MCP helpers: field reader and reply parser (no python3: skipped the server round trip)"
fi

home="${work}/home"
mkdir -p "${home}/.fleet"
printf 'secret-token' > "${home}/.fleet/mcp.token"
log="${work}/requests.log"
: > "${log}"

# The server binds after a second and only then writes mcp.port, like a
# daemon still starting: itest_mcp_call has to wait for it. It logs one line
# per request: <http method> <json-rpc method or tool name> <session> <auth>.
cat > "${work}/server.py" <<'PY'
import http.server, json, sys, time
port_file, log_file = sys.argv[1], sys.argv[2]
time.sleep(1)

class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def note(self, what):
        with open(log_file, "a") as f:
            f.write("%s %s %s %s\n" % (self.command, what,
                    self.headers.get("Mcp-Session-Id", "-"),
                    self.headers.get("Authorization", "-")))

    def reply(self, code, body=b"", headers=()):
        self.send_response(code)
        for k, v in headers:
            self.send_header(k, v)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_DELETE(self):
        self.note("close")
        self.reply(200)

    def do_POST(self):
        msg = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        method = msg.get("method")
        if self.headers.get("Authorization") != "Bearer secret-token":
            self.note(method)
            return self.reply(401)
        if method == "initialize":
            self.note(method)
            out = {"jsonrpc": "2.0", "id": msg["id"], "result": {"protocolVersion": "2025-06-18"}}
            return self.reply(200, ("event: message\ndata: %s\n\n" % json.dumps(out)).encode(),
                              [("Mcp-Session-Id", "sess-1"), ("Content-Type", "text/event-stream")])
        if self.headers.get("Mcp-Session-Id") != "sess-1":
            self.note(method)
            return self.reply(404)
        if method == "notifications/initialized":
            self.note(method)
            return self.reply(202)
        name = msg["params"]["name"]
        self.note(name)
        if name == "boom":
            out = {"jsonrpc": "2.0", "id": msg["id"], "error": {"code": -32602, "message": "no such tool"}}
            return self.reply(200, json.dumps(out).encode(), [("Content-Type", "application/json")])
        result = {"structuredContent": {"tool": name, "args": msg["params"]["arguments"]}}
        out = {"jsonrpc": "2.0", "id": msg["id"], "result": result}
        return self.reply(200, ("event: message\ndata: %s\n\n" % json.dumps(out)).encode(),
                          [("Content-Type", "text/event-stream")])

srv = http.server.HTTPServer(("127.0.0.1", 0), H)
with open(port_file + ".tmp", "w") as f:
    f.write(str(srv.server_address[1]))
import os
os.rename(port_file + ".tmp", port_file)
srv.serve_forever()
PY
python3 "${work}/server.py" "${home}/.fleet/mcp.port" "${log}" &
SERVER_PID=$!

info "itest_mcp_call waits for the listener, then does the whole handshake"
out=$(HOME="${home}" itest_mcp_call fleet_echo '{"word":"a b","n":3}' structuredContent.args.word) \
  || fail "itest_mcp_call did not get through to the stand-in server"
assert_equals "a b" "${out}" "the tool saw the arguments, and the field was read from its result"
assert_equals "$(printf '%s\n' \
  "POST initialize - Bearer secret-token" \
  "POST notifications/initialized sess-1 Bearer secret-token" \
  "POST fleet_echo sess-1 Bearer secret-token" \
  "DELETE close sess-1 Bearer secret-token")" "$(cat "${log}")" "the request sequence"

out=$(HOME="${home}" itest_mcp_call fleet_echo '{}')
assert_equals '{"structuredContent":{"args":{},"tool":"fleet_echo"}}' "${out}" "with no field, the whole result"

info "a JSON-RPC error fails at once, without retrying"
: > "${log}"
start=$(date +%s)
if HOME="${home}" itest_mcp_call boom '{}' >/dev/null 2>&1; then
  fail "itest_mcp_call should fail on a JSON-RPC error"
fi
assert_equals "1" "$(grep -c '^POST boom ' "${log}")" "the failing call should be made once"
[ $(( $(date +%s) - start )) -lt 5 ] || fail "a JSON-RPC error should not be retried for seconds"

pass "itest MCP helpers: field reader, reply parser and the handshake against a stand-in server"
