import json
import select
import subprocess
import time
from pathlib import Path

command = [
    "/tmp/fleet-codex-evidence/fleet", "exec",
    "codex-auto-evidence/auto-default", "--", "bash", "-lc",
    "codex app-server",
]
with open("/tmp/fleet-codex-evidence/app-server.stderr", "w") as stderr:
    process = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=stderr)
    def send(message):
        process.stdin.write((json.dumps(message) + "\n").encode())
        process.stdin.flush()
    send({"id": 1, "method": "initialize", "params": {
        "clientInfo": {"name": "fleet-mode-evidence", "version": "1.0.0"},
        "capabilities": {"experimentalApi": True},
    }})
    deadline = time.monotonic() + 40
    buffer = b""
    try:
        while time.monotonic() < deadline:
            if not select.select([process.stdout], [], [], 1)[0]:
                continue
            chunk = process.stdout.read1(65536)
            if not chunk:
                raise RuntimeError("Codex app-server exited before responding")
            buffer += chunk
            while b"\n" in buffer:
                line, buffer = buffer.split(b"\n", 1)
                try:
                    message = json.loads(line)
                except ValueError:
                    continue
                if message.get("id") == 1:
                    if "error" in message:
                        raise RuntimeError(message["error"])
                    send({"method": "initialized"})
                    send({"id": 2, "method": "config/read", "params": {"includeLayers": True}})
                if message.get("id") == 2:
                    if "error" in message:
                        raise RuntimeError(message["error"])
                    result = message["result"]
                    Path("/tmp/fleet-codex-evidence/config-read.json").write_text(json.dumps(result, indent=2) + "\n")
                    config = result["config"]
                    print("Codex config/read (no permission overrides; no login):")
                    for key in ("approval_policy", "approvals_reviewer", "sandbox_mode"):
                        print(f"  {key} = {json.dumps(config[key])}")
                    assert config["approval_policy"] == "on-request"
                    assert config["approvals_reviewer"] == "auto_review"
                    assert config["sandbox_mode"] == "workspace-write"
                    print("PASS: Codex loaded automatic approval review by default.")
                    raise SystemExit(0)
        raise TimeoutError("Timed out waiting for Codex config/read")
    finally:
        process.stdin.close()
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.terminate()
