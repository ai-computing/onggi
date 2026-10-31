"""Thin wrapper over the `orca` CLI.

Remote nodes are reached only through Orca: Orca already holds the SSH sessions (password auth),
so the hub runs a command by opening a short-lived terminal in the project's worktree, reading
its output back between nonce markers, and closing the tab.
"""
from __future__ import annotations

import base64
import json
import shutil
import subprocess
import time
import uuid

CHUNK = 1500  # characters per line typed into a remote shell


class OrcaError(RuntimeError):
    pass


def _orca_bin() -> str:
    path = shutil.which("orca")
    if not path:
        raise OrcaError("`orca` CLI not found on PATH")
    return path


def call(*args: str, timeout: float = 60) -> dict:
    """Run `orca <args> --json` and return the `result` object."""
    proc = subprocess.run(
        [_orca_bin(), *args, "--json"],
        capture_output=True, timeout=timeout, encoding="utf-8", errors="replace",
    )
    try:
        payload = json.loads(proc.stdout)
    except json.JSONDecodeError:
        raise OrcaError(f"orca {' '.join(args)}: {proc.stderr.strip() or proc.stdout.strip()}")
    if not payload.get("ok"):
        raise OrcaError(f"orca {' '.join(args)}: {payload.get('error') or payload}")
    return payload["result"]


def worktrees() -> list[dict]:
    return call("worktree", "ps")["worktrees"]


def hosts() -> dict[str, str]:
    """Map host label (`xeon_max`, `local`) to Orca host id (`ssh:...`, `local`)."""
    out = subprocess.run([_orca_bin(), "host", "list"], capture_output=True,
                         encoding="utf-8", errors="replace").stdout
    labels = {}
    for line in out.splitlines():
        if "--host " not in line:
            continue
        host_id = line.rsplit("--host ", 1)[1].strip()
        parts = line.split()
        if parts[0] == "local":
            labels["local"] = host_id
        elif parts[:2] == ["ssh", "target"]:
            labels[parts[2]] = host_id
    return labels


def find_worktree(trees: list[dict], host_id: str, path: str) -> dict | None:
    norm = lambda p: p.replace("\\", "/").rstrip("/").lower()
    for w in trees:
        if w.get("hostId") == host_id and norm(w.get("path", "")) == norm(path):
            return w
    return None


def run_remote(worktree_id: str, script: str, timeout: float = 120) -> tuple[int, str]:
    """Run a bash script in the worktree's directory on its (Linux) host.

    Returns (exit code, combined stdout/stderr). The script travels base64-encoded and the
    output comes back base64-encoded in short lines, so quoting and terminal rendering can't
    corrupt either; the end marker carries the byte count to catch dropped lines.
    """
    nonce = uuid.uuid4().hex[:10]
    b64 = base64.b64encode(script.encode()).decode()
    tmp = f"/tmp/.rc-{nonce}"
    lines = [f"printf '' > {tmp}.b64"]
    lines += [f"printf %s '{b64[i:i + CHUNK]}' >> {tmp}.b64" for i in range(0, len(b64), CHUNK)]
    # Markers are produced by printf so the echoed command line never contains them verbatim.
    lines.append(
        f"base64 -d {tmp}.b64 > {tmp}.sh; bash {tmp}.sh > {tmp}.out 2>&1; rc=$?; "
        f"printf '__%s_%s__\\n' RCB {nonce}; base64 -w 100 {tmp}.out; "
        f"printf '__%s_%s_%s_%s__\\n' RCE {nonce} $rc $(wc -c < {tmp}.out); "
        f"rm -f {tmp}.b64 {tmp}.sh {tmp}.out"
    )
    term = call("terminal", "create", "--worktree", f"id:{worktree_id}",
                "--title", "research-hub", "--command", lines[0])["terminal"]
    handle = term["handle"]
    try:
        for line in lines[1:]:
            call("terminal", "send", "--terminal", handle, "--text", line, "--enter")
        begin, end = f"__RCB_{nonce}__", f"__RCE_{nonce}_"
        seen: list[str] = []
        cursor = "0"  # cursor reads return the full range; a cursor-less read is capped
        deadline = time.time() + timeout
        while time.time() < deadline:
            out = call("terminal", "read", "--terminal", handle, "--cursor", cursor,
                       "--limit", "5000")["terminal"]
            seen += out["tail"]
            cursor = out.get("nextCursor", cursor)
            stripped = [l.strip() for l in seen]
            ends = [i for i, l in enumerate(stripped) if l.startswith(end)]
            starts = [i for i, l in enumerate(stripped) if l == begin]
            if ends and starts:
                rc, size = (int(x) for x in stripped[ends[-1]][len(end):].rstrip("_").split("_"))
                data = base64.b64decode("".join(stripped[starts[-1] + 1:ends[-1]]))
                if len(data) != size:
                    raise OrcaError(f"remote output truncated in transit ({len(data)}/{size} bytes)")
                return rc, data.decode("utf-8", "replace")
            time.sleep(0.5)
        raise OrcaError(f"timed out after {timeout}s waiting for remote command in {worktree_id}")
    finally:
        try:
            call("terminal", "close", "--terminal", handle, "--tab")
        except Exception:
            pass


def set_comment(worktree_id: str, comment: str) -> None:
    call("worktree", "set", "--worktree", f"id:{worktree_id}", "--comment", comment)


def launch_agent(worktree_id: str, command: str, title: str) -> str:
    term = call("terminal", "create", "--worktree", f"id:{worktree_id}",
                "--title", title, "--command", command)["terminal"]
    return term["handle"]
