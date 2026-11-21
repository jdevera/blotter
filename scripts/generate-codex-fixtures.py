#!/usr/bin/env python3
"""Capture hashes produced by codex-rs itself, without a model or credentials."""
import json
import os
from pathlib import Path
import select
import subprocess
import tempfile
import time


def capture(home, cwd):
    env = dict(os.environ, CODEX_HOME=str(home), HOME=str(cwd))
    process = subprocess.Popen(
        ["codex", "app-server"], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL, env=env, cwd=cwd, text=True, bufsize=1,
    )

    def call(request_id, method, params):
        process.stdin.write(json.dumps({"id": request_id, "method": method, "params": params}) + "\n")
        process.stdin.flush()
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            if not select.select([process.stdout], [], [], 1)[0]:
                continue
            line = process.stdout.readline()
            if not line:
                raise RuntimeError("Codex app-server exited")
            reply = json.loads(line)
            if reply.get("id") == request_id:
                if "error" in reply:
                    raise RuntimeError(reply["error"])
                return reply["result"]
        raise TimeoutError(method)

    try:
        call(1, "initialize", {"clientInfo": {"name": "blotter-fixtures", "version": "1"}, "capabilities": {"experimentalApi": True}})
        process.stdin.write('{"method":"initialized"}\n')
        process.stdin.flush()
        return call(2, "hooks/list", {"cwds": [str(cwd)]})["data"][0]["hooks"]
    finally:
        process.terminate()
        process.wait(timeout=5)


def main():
    version = subprocess.check_output(["codex", "--version"], text=True).strip()
    if version != "codex-cli 0.171.0":
        raise RuntimeError(f"Expected codex-cli 0.171.0, got {version}")
    cases = [
        {"name": "absent-matcher", "handler": {"type": "command", "command": "/bin/echo yes"}},
        {"name": "empty-matcher", "matcher": "", "handler": {"type": "command", "command": "/bin/echo yes"}},
        {"name": "status", "matcher": "Bash", "handler": {"type": "command", "command": "/bin/echo yes", "statusMessage": "check <&> café\u2028"}},
        {"name": "custom-limit", "handler": {"type": "command", "command": "/bin/echo yes", "additionalContextLimit": 20000}},
        {"name": "default-limit", "handler": {"type": "command", "command": "/bin/echo yes", "additionalContextLimit": 2500}},
        {"name": "zero-limit", "handler": {"type": "command", "command": "/bin/echo yes", "additionalContextLimit": 0}},
        {"name": "async", "handler": {"type": "command", "command": "/bin/echo yes", "async": True}},
        {"name": "zero-timeout", "handler": {"type": "command", "command": "/bin/echo yes", "timeout": 0}},
        {"name": "combined", "matcher": "", "handler": {"type": "command", "command": "/bin/echo 'a b'", "async": True, "timeout": 9, "statusMessage": "checking", "additionalContextLimit": 0}},
    ]
    with tempfile.TemporaryDirectory(prefix="blotter-codex-fixtures-") as temp:
        cwd = Path(temp).resolve()
        home = cwd / "codex"
        home.mkdir()
        real_hooks = cwd / "actual-hooks.json"
        (home / "hooks.json").symlink_to(real_hooks)
        groups = []
        for case in cases:
            group = {"hooks": [case["handler"]]}
            if "matcher" in case:
                group["matcher"] = case["matcher"]
            groups.append(group)
        real_hooks.write_text(json.dumps({"hooks": {"PreToolUse": groups}}))
        hooks = capture(home, cwd)
        assert len(hooks) == len(cases), hooks
        for i, (case, hook) in enumerate(zip(cases, hooks)):
            assert hook["key"] == f"{home}/hooks.json:pre_tool_use:{i}:0", hook
            case["hash"] = hook["currentHash"]
        result = {
            "producer": version,
            "source_tag": "rust-v0.171.0",
            "symlink_path_preserved": True,
            "cases": cases,
        }
    path = Path(__file__).resolve().parents[1] / "internal/install/testdata/codex-hashes.json"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(result, ensure_ascii=False, indent=2) + "\n")
    print(path)


if __name__ == "__main__":
    main()
