"""Kiln: isolated Linux microVMs.

The handwritten layer. It adds what a generator does badly: streaming exec,
file transfer, and a sandbox object that reads like the thing it names.
"""

from __future__ import annotations

import json
import time
from dataclasses import dataclass, field
from typing import Any, Callable, Iterator, Optional

from ._generated import GeneratedClient, KilnError

__all__ = ["Kiln", "Sandbox", "ExecResult", "ExecChunk", "KilnError"]


@dataclass
class ExecChunk:
    """One piece of output from a running command."""

    stream: str
    text: str


@dataclass
class ExecResult:
    """What a finished command answers."""

    exit_code: int = 0
    stdout: str = ""
    stderr: str = ""
    truncated: bool = False
    timed_out: bool = False
    chunks: list[ExecChunk] = field(default_factory=list)


class Kiln(GeneratedClient):
    """The client. Every call names the tenant of its API key."""

    def sandbox(self, sandbox_id: str) -> "Sandbox":
        """Open one sandbox by id."""
        return Sandbox(self, sandbox_id)

    def create_sandbox_object(self, **body: Any) -> "Sandbox":
        """Create a sandbox and return it as an object."""
        created = self.create_sandbox(**body)
        return Sandbox(self, created["id"])

    def build_template(self, *, poll_seconds: float = 2.0, timeout_seconds: float = 900.0, **body: Any) -> None:
        """Build a template and wait until it is ready."""
        self.create_template(**body)
        deadline = time.monotonic() + timeout_seconds
        while True:
            template = self.get_template(body["name"])
            if template["state"] == "ready":
                return
            if template["state"] == "failed":
                raise KilnError(409, "conflict", template.get("error", "the template build failed"))
            if time.monotonic() > deadline:
                raise KilnError(409, "conflict", f"the template {body['name']} is still building")
            time.sleep(poll_seconds)


class Sandbox:
    """One sandbox."""

    def __init__(self, client: Kiln, sandbox_id: str) -> None:
        self.client = client
        self.id = sandbox_id

    def exec(
        self,
        cmd: list[str],
        *,
        cwd: Optional[str] = None,
        env: Optional[dict[str, str]] = None,
        timeout_seconds: Optional[int] = None,
        on_output: Optional[Callable[[ExecChunk], None]] = None,
    ) -> ExecResult:
        """Run a command. With on_output the call streams as it runs."""
        body: dict[str, Any] = {"cmd": cmd}
        if cwd is not None:
            body["cwd"] = cwd
        if env is not None:
            body["env"] = env
        if timeout_seconds is not None:
            body["timeout_seconds"] = timeout_seconds
        if on_output is None:
            raw = self.client.exec(self.id, cmd, cwd=cwd, env=env, timeout_seconds=timeout_seconds)
            return ExecResult(
                exit_code=raw.get("exit_code", 0),
                stdout=raw.get("stdout", ""),
                stderr=raw.get("stderr", ""),
                truncated=raw.get("truncated", False),
                timed_out=raw.get("timed_out", False),
            )
        result = ExecResult()
        for chunk in self._stream_exec(body):
            if isinstance(chunk, ExecResult):
                result.exit_code = chunk.exit_code
                result.timed_out = chunk.timed_out
                continue
            on_output(chunk)
            result.chunks.append(chunk)
            if chunk.stream == "stderr":
                result.stderr += chunk.text
            else:
                result.stdout += chunk.text
        return result

    def _stream_exec(self, body: dict[str, Any]) -> Iterator[Any]:
        """Read the event stream of one exec."""
        url = f"{self.client.base_url}/v1/sandboxes/{self.id}/exec"
        headers = {
            "Authorization": f"Bearer {self.client.api_key}",
            "Accept": "text/event-stream",
        }
        with self.client._client.stream("POST", url, json=body, headers=headers) as response:
            if response.status_code >= 400:
                response.read()
                raise KilnError(response.status_code, "internal", response.text)
            event = ""
            for line in response.iter_lines():
                line = line.rstrip("\r")
                if line.startswith("event:"):
                    event = line[6:].strip()
                    continue
                if not line.startswith("data:"):
                    continue
                data = line[5:].strip()
                if event == "exit":
                    finished = json.loads(data)
                    yield ExecResult(
                        exit_code=finished.get("exit_code", 0),
                        timed_out=finished.get("timed_out", False),
                    )
                    continue
                if event == "error":
                    raise KilnError(500, "internal", json.loads(data))
                yield ExecChunk(stream="stderr" if event == "stderr" else "stdout", text=json.loads(data))

    def read_file(self, path: str) -> bytes:
        """Read one file out of the sandbox."""
        response = self.client._request("GET", f"/v1/sandboxes/{self.id}/files/{path.lstrip('/')}")
        return response.content

    def write_file(self, path: str, body: bytes | str) -> None:
        """Write one file into the sandbox."""
        payload = body.encode() if isinstance(body, str) else body
        self.client._request(
            "PUT", f"/v1/sandboxes/{self.id}/files/{path.lstrip('/')}", content=payload
        )

    def publish(self, port: int, visibility: str = "public") -> Any:
        return self.client.publish(self.id, port, visibility)

    def sleep(self) -> Any:
        """Snapshot and stop. The sandbox keeps its id and its hostnames."""
        return self.client.snapshot(self.id, stop=True)

    def fork(self, count: int) -> Any:
        return self.client.fork(self.id, count)

    def destroy(self) -> None:
        self.client.delete_sandbox(self.id)
