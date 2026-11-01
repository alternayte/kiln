"""Generated from internal/apispec. Do not edit.

Run: just sdk
"""

from __future__ import annotations

import urllib.parse
from typing import Any, Optional

import httpx


class KilnError(Exception):
    """One failed call. The code is stable; the message names the next step."""

    def __init__(self, status: int, code: str, message: str) -> None:
        super().__init__(message)
        self.status = status
        self.code = code
        self.message = message


class GeneratedClient:
    def __init__(self, base_url: str, api_key: str, client: Optional[httpx.Client] = None) -> None:
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key
        self._client = client or httpx.Client(timeout=httpx.Timeout(60.0, read=None))

    def close(self) -> None:
        self._client.close()

    def _request(self, method: str, path: str, body: Any = None, **kwargs: Any) -> httpx.Response:
        headers = {"Authorization": f"Bearer {self.api_key}"}
        headers.update(kwargs.pop("headers", {}) or {})
        response = self._client.request(
            method, self.base_url + path, json=body, headers=headers, **kwargs
        )
        if response.status_code >= 400:
            code, message = "internal", response.reason_phrase
            try:
                failure = response.json().get("error", {})
                code = failure.get("code", code)
                message = failure.get("message", message)
            except ValueError:
                pass
            raise KilnError(response.status_code, code, message)
        return response

    def _json(self, method: str, path: str, body: Any = None) -> Any:
        response = self._request(method, path, body)
        if response.status_code == 204 or not response.content:
            return None
        return response.json()

    def health(self) -> Any:
        """Report whether the host can run sandboxes."""
        return self._json("GET", "/v1/health")

    def list_registries(self) -> Any:
        """List the registry credentials of this tenant. The tokens are never returned."""
        return self._json("GET", "/v1/registries")

    def put_registry(self, host: str, username: str, token: str) -> Any:
        """Store the credential a template build uses to pull a private image for this tenant."""
        body = {k: v for k, v in {
            "host": host,
            "username": username,
            "token": token,
        }.items() if v is not None}
        return self._json("POST", "/v1/registries", body)

    def delete_registry(self, host: str) -> Any:
        """Remove one registry credential."""
        return self._json("DELETE", f"/v1/registries/{host}")

    def list_sandboxes(self) -> Any:
        """List the sandboxes of this tenant."""
        return self._json("GET", "/v1/sandboxes")

    def create_sandbox(self, template: str, lifecycle: str, idle_seconds: int, ttl_seconds: Optional[int] = None, metadata: Optional[dict[str, Any]] = None, secrets: Optional[list[str]] = None, env: Optional[dict[str, Any]] = None) -> Any:
        """Create a sandbox by restoring a template snapshot."""
        body = {k: v for k, v in {
            "template": template,
            "lifecycle": lifecycle,
            "ttl_seconds": ttl_seconds,
            "idle_seconds": idle_seconds,
            "metadata": metadata,
            "secrets": secrets,
            "env": env,
        }.items() if v is not None}
        return self._json("POST", "/v1/sandboxes", body)

    def delete_sandbox(self, id: str) -> Any:
        """Destroy a sandbox and retire its hostnames."""
        return self._json("DELETE", f"/v1/sandboxes/{id}")

    def get_sandbox(self, id: str) -> Any:
        """Read one sandbox with its published ports."""
        return self._json("GET", f"/v1/sandboxes/{id}")

    def set_egress(self, id: str, egress_allow: Optional[list[str]] = None, observe: Optional[bool] = None) -> Any:
        """Replace the egress policy of one sandbox. Send egress_allow or observe. The answer comes after the host swaps the policy and cuts every connection the guest opened. A connection to a published port stays. A sleeping sandbox takes the change without a wake, and a later snapshot or fork carries it."""
        body = {k: v for k, v in {
            "egress_allow": egress_allow,
            "observe": observe,
        }.items() if v is not None}
        return self._json("PUT", f"/v1/sandboxes/{id}/egress", body)

    def egress_log(self, id: str) -> Any:
        """Read the egress record: one row per destination, with its decision, counts and bytes. A row names a host, or an address when the connection named no host. A fork or a restore starts with no rows. The rows stay for 7 days after a destroy."""
        return self._json("GET", f"/v1/sandboxes/{id}/egress/log")

    def exec(self, id: str, cmd: list[str], cwd: Optional[str] = None, env: Optional[dict[str, Any]] = None, timeout_seconds: Optional[int] = None) -> Any:
        """Run a command inside a sandbox. Accept text/event-stream to read the output as it runs."""
        body = {k: v for k, v in {
            "cmd": cmd,
            "cwd": cwd,
            "env": env,
            "timeout_seconds": timeout_seconds,
        }.items() if v is not None}
        return self._json("POST", f"/v1/sandboxes/{id}/exec", body)

    def fork(self, id: str, count: int, allow_secret_fork: Optional[bool] = None, metadata: Optional[dict[str, Any]] = None) -> Any:
        """Snapshot a running sandbox and restore several copies of it."""
        body = {k: v for k, v in {
            "count": count,
            "allow_secret_fork": allow_secret_fork,
            "metadata": metadata,
        }.items() if v is not None}
        return self._json("POST", f"/v1/sandboxes/{id}/fork", body)

    def logs(self, id: str) -> Any:
        """Read what the guest kept of the start command output, how the start command ended, and the template build result. A sleeping sandbox wakes first."""
        return self._json("GET", f"/v1/sandboxes/{id}/logs")

    def start_process(self, id: str, cmd: list[str], cwd: Optional[str] = None, env: Optional[dict[str, Any]] = None) -> Any:
        """Start a detached process and answer once it runs. It has no deadline, it outlives this request, and the sandbox does not sleep while it runs."""
        body = {k: v for k, v in {
            "cmd": cmd,
            "cwd": cwd,
            "env": env,
        }.items() if v is not None}
        return self._json("POST", f"/v1/sandboxes/{id}/processes", body)

    def signal_process(self, id: str, pid: str, signal: Optional[str] = None) -> Any:
        """Send a signal to the process group of a detached process. A process that has ended takes no signal, and the call still succeeds."""
        query = urllib.parse.urlencode({k: v for k, v in {
            "signal": signal,
        }.items() if v is not None})
        return self._json("DELETE", f"/v1/sandboxes/{id}/processes/{pid}" + ("?" + query if query else ""))

    def read_process(self, id: str, pid: str, stdout_offset: Optional[int] = None, stderr_offset: Optional[int] = None) -> Any:
        """Read the state of a detached process and its output after each offset. One answer holds at most 1 MiB of each stream, the guest keeps the last 8 MiB, and the answer names the next offsets and the bytes lost."""
        query = urllib.parse.urlencode({k: v for k, v in {
            "stdout_offset": stdout_offset,
            "stderr_offset": stderr_offset,
        }.items() if v is not None})
        return self._json("GET", f"/v1/sandboxes/{id}/processes/{pid}" + ("?" + query if query else ""))

    def publish(self, id: str, port: int, visibility: str) -> Any:
        """Serve a guest port on a public hostname."""
        body = {k: v for k, v in {
            "port": port,
            "visibility": visibility,
        }.items() if v is not None}
        return self._json("POST", f"/v1/sandboxes/{id}/publish", body)

    def retire(self, id: str, port: int) -> Any:
        """Retire a published hostname. It answers 404 from then on."""
        return self._json("DELETE", f"/v1/sandboxes/{id}/publish/{port}")

    def snapshot(self, id: str, stop: Optional[bool] = None) -> Any:
        """Snapshot a sandbox. With stop the sandbox sleeps and keeps its hostnames."""
        body = {k: v for k, v in {
            "stop": stop,
        }.items() if v is not None}
        return self._json("POST", f"/v1/sandboxes/{id}/snapshot", body)

    def list_snapshots(self) -> Any:
        """List the snapshots of this tenant."""
        return self._json("GET", "/v1/snapshots")

    def delete_snapshot(self, id: str) -> Any:
        """Delete a snapshot that has no live sandbox."""
        return self._json("DELETE", f"/v1/snapshots/{id}")

    def get_snapshot(self, id: str) -> Any:
        """Read one snapshot."""
        return self._json("GET", f"/v1/snapshots/{id}")

    def restore(self, id: str, count: int, allow_secret_fork: Optional[bool] = None, metadata: Optional[dict[str, Any]] = None) -> Any:
        """Restore a snapshot into one or more new sandboxes."""
        body = {k: v for k, v in {
            "count": count,
            "allow_secret_fork": allow_secret_fork,
            "metadata": metadata,
        }.items() if v is not None}
        return self._json("POST", f"/v1/snapshots/{id}/restore", body)

    def list_templates(self) -> Any:
        """List the templates of this tenant."""
        return self._json("GET", "/v1/templates")

    def create_template(self, name: str, image: str, vcpus: int, memory_mb: int, disk_mb: int, egress_allow: list[str], setup: Optional[list[str]] = None, ttl_seconds: Optional[int] = None, start: Optional[list[str]] = None, port: Optional[int] = None, services: Optional[list[dict[str, Any]]] = None, seed: Optional[list[str]] = None, user: Optional[str] = None, credential: Optional[dict[str, Any]] = None) -> Any:
        """Build a template from an OCI image. The build runs after the answer."""
        body = {k: v for k, v in {
            "name": name,
            "image": image,
            "vcpus": vcpus,
            "memory_mb": memory_mb,
            "disk_mb": disk_mb,
            "setup": setup,
            "egress_allow": egress_allow,
            "ttl_seconds": ttl_seconds,
            "start": start,
            "port": port,
            "services": services,
            "seed": seed,
            "user": user,
            "credential": credential,
        }.items() if v is not None}
        return self._json("POST", "/v1/templates", body)

    def delete_template(self, name: str) -> Any:
        """Delete a template that has no live sandbox and no snapshot."""
        return self._json("DELETE", f"/v1/templates/{name}")

    def get_template(self, name: str) -> Any:
        """Read one template."""
        return self._json("GET", f"/v1/templates/{name}")

    def list_viewers(self) -> Any:
        """List the viewers of this tenant."""
        return self._json("GET", "/v1/viewers")

    def create_viewer(self, email: str, password: str) -> Any:
        """Add a viewer of this tenant. A viewer opens team previews and nothing else."""
        body = {k: v for k, v in {
            "email": email,
            "password": password,
        }.items() if v is not None}
        return self._json("POST", "/v1/viewers", body)

    def delete_viewer(self, id: str) -> Any:
        """Revoke one viewer. The viewer is disabled and its sessions end."""
        return self._json("DELETE", f"/v1/viewers/{id}")
