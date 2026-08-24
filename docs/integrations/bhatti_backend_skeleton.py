"""bhatti microVM execution environment — DRAFT skeleton for hermes-agent.

Drop into hermes-agent as ``tools/environments/bhatti.py``. Modeled on
``daytona.py``: spawn-per-call via ``_ThreadedProcessHandle`` wrapping blocking
REST calls; ``cancel_fn`` wired to sandbox stop for interrupt support; persistent
filesystem via bhatti stop(=snapshot)/start(=resume).

bhatti REST surface used (see https://bhatti.sh/docs/reference/api):
  POST   /sandboxes                         create  {name,cpus,memory_mb,env}
  GET    /sandboxes                          list (resume-by-name lookup)
  POST   /sandboxes/:id/exec                 {"cmd":[...]} -> {exit_code,stdout,stderr}
  POST   /sandboxes/:id/stop                 snapshot+pause (persistent)
  POST   /sandboxes/:id/start                resume
  DELETE /sandboxes/:id                      destroy (ephemeral)
  GET    /sandboxes/:id/files?path=          read
  PUT    /sandboxes/:id/files?path=          write

This is a planning artifact; it is intentionally light on edge-case handling so
the shape is readable. Harden (retries, 409 handling, streaming) during impl.
"""

import json
import logging
import os
import shlex
import threading
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

from tools.environments.base import BaseEnvironment, _ThreadedProcessHandle
from tools.environments.file_sync import (
    FileSyncManager,
    iter_sync_files,
)

logger = logging.getLogger(__name__)


class _BhattiClient:
    """Tiny stdlib REST client — zero new dependencies."""

    def __init__(self, base_url: str, token: str, timeout: float = 60.0):
        self.base = base_url.rstrip("/")
        self.token = token
        self.timeout = timeout

    def _req(self, method: str, path: str, *, body=None, raw: bytes | None = None,
             headers: dict | None = None, timeout: float | None = None):
        url = f"{self.base}{path}"
        data = raw if raw is not None else (
            json.dumps(body).encode() if body is not None else None
        )
        hdrs = {"Authorization": f"Bearer {self.token}"}
        if body is not None:
            hdrs["Content-Type"] = "application/json"
        hdrs.update(headers or {})
        req = urllib.request.Request(url, data=data, method=method, headers=hdrs)
        with urllib.request.urlopen(req, timeout=timeout or self.timeout) as resp:
            payload = resp.read()
            ctype = resp.headers.get("Content-Type", "")
            if "application/json" in ctype:
                return json.loads(payload or b"{}")
            return payload

    # --- typed helpers -------------------------------------------------
    def create(self, name, cpus, memory_mb, env):
        return self._req("POST", "/sandboxes", body={
            "name": name, "cpus": cpus, "memory_mb": memory_mb, "env": env or {},
        })

    def list(self):
        return self._req("GET", "/sandboxes")

    def exec(self, sid, argv, timeout):
        return self._req("POST", f"/sandboxes/{sid}/exec",
                         body={"cmd": argv}, timeout=timeout + 5)

    def stop(self, sid):
        return self._req("POST", f"/sandboxes/{sid}/stop")

    def start(self, sid):
        return self._req("POST", f"/sandboxes/{sid}/start")

    def destroy(self, sid):
        return self._req("DELETE", f"/sandboxes/{sid}")

    def write_file(self, sid, path, content: bytes):
        q = urllib.parse.urlencode({"path": path})
        self._req("PUT", f"/sandboxes/{sid}/files?{q}", raw=content,
                  headers={"Content-Type": "application/octet-stream"})

    def read_file(self, sid, path) -> bytes:
        q = urllib.parse.urlencode({"path": path})
        return self._req("GET", f"/sandboxes/{sid}/files?{q}")


class BhattiEnvironment(BaseEnvironment):
    """bhatti Firecracker microVM backend.

    Persistent filesystem: on cleanup the sandbox is stopped (snapshotted) and
    on next construction with the same task_id it is resumed in microseconds.
    """

    _stdin_mode = "heredoc"      # REST exec has no stdin pipe
    _snapshot_timeout = 60       # generous for first cold create

    def __init__(self, image: str, cwd: str = "/root", timeout: int = 60,
                 cpu: int = 1, memory: int = 1024,
                 persistent_filesystem: bool = True, task_id: str = "default"):
        super().__init__(cwd=cwd, timeout=timeout)

        url = os.getenv("TERMINAL_BHATTI_URL")
        token = os.getenv("BHATTI_TOKEN") or os.getenv("TERMINAL_BHATTI_TOKEN")
        if not url or not token:
            raise ValueError(
                "bhatti backend requires TERMINAL_BHATTI_URL and BHATTI_TOKEN")

        self._client = _BhattiClient(url, token)
        self._persistent = persistent_filesystem
        self._task_id = task_id
        self._lock = threading.Lock()
        self._name = f"hermes-{task_id}"

        # Resume-by-name (persistent) or create fresh.
        self._sid = None
        if self._persistent:
            try:
                for sb in (self._client.list() or []):
                    if sb.get("name") == self._name:
                        self._sid = sb["id"]
                        if sb.get("status") != "running":
                            self._client.start(self._sid)
                        logger.info("bhatti: resumed sandbox %s", self._sid)
                        break
            except Exception as e:
                logger.warning("bhatti: resume lookup failed: %s", e)

        if self._sid is None:
            sb = self._client.create(
                name=self._name, cpus=int(cpu),
                memory_mb=int(memory), env={})
            self._sid = sb["id"]
            logger.info("bhatti: created sandbox %s (image=%s)", self._sid, image)

        # Resolve $HOME and seed file sync into /root/.hermes.
        self._remote_home = "/root"
        try:
            r = self._client.exec(self._sid, ["bash", "-lc", "echo $HOME"], 10)
            home = (r.get("stdout") or "").strip()
            if home:
                self._remote_home = home
                if cwd in {"~", "/root"}:
                    self.cwd = home
        except Exception:
            pass

        self._sync = FileSyncManager(
            get_files_fn=lambda: iter_sync_files(f"{self._remote_home}/.hermes"),
            upload_fn=self._upload,
            delete_fn=self._delete,
        )
        self._sync.sync(force=True)
        self.init_session()

    # --- file sync transport ------------------------------------------
    def _upload(self, host_path: str, remote_path: str) -> None:
        parent = str(Path(remote_path).parent)
        self._client.exec(self._sid, ["mkdir", "-p", parent], 15)
        self._client.write_file(self._sid, remote_path,
                                Path(host_path).read_bytes())

    def _delete(self, remote_paths: list[str]) -> None:
        if remote_paths:
            self._client.exec(self._sid, ["rm", "-f", *remote_paths], 15)

    # --- lifecycle / exec ---------------------------------------------
    def _ensure_ready(self) -> None:
        try:
            for sb in (self._client.list() or []):
                if sb.get("id") == self._sid and sb.get("status") != "running":
                    self._client.start(self._sid)
                    return
        except Exception as e:
            logger.warning("bhatti: ensure_ready failed: %s", e)

    def _before_execute(self) -> None:
        with self._lock:
            self._ensure_ready()
        self._sync.sync()

    def _run_bash(self, cmd_string: str, *, login: bool = False,
                  timeout: int = 120, stdin_data: str | None = None):
        sid, client, lock = self._sid, self._client, self._lock
        flag = "-lc" if login else "-c"
        argv = ["bash", flag, cmd_string]

        def cancel():
            with lock:
                try:
                    client.stop(sid)
                except Exception:
                    pass

        def exec_fn() -> tuple[str, int]:
            resp = client.exec(sid, argv, timeout)
            out = (resp.get("stdout") or "") + (resp.get("stderr") or "")
            return out, int(resp.get("exit_code", 0))

        return _ThreadedProcessHandle(exec_fn, cancel_fn=cancel)

    def cleanup(self):
        with self._lock:
            if self._sid is None:
                return
            try:
                self._sync.sync_back()
            except Exception as e:
                logger.warning("bhatti: sync_back failed: %s", e)
            try:
                if self._persistent:
                    self._client.stop(self._sid)   # snapshot, resume later
                    logger.info("bhatti: stopped %s (fs preserved)", self._sid)
                else:
                    self._client.destroy(self._sid)
                    logger.info("bhatti: destroyed %s", self._sid)
            except Exception as e:
                logger.warning("bhatti: cleanup failed: %s", e)
            self._sid = None

