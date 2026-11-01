// Generated from internal/apispec. Do not edit.
// Run: just sdk

export class KilnError extends Error {
  readonly code: string;
  readonly status: number;
  constructor(status: number, code: string, message: string) {
    super(message);
    this.name = "KilnError";
    this.code = code;
    this.status = status;
  }
}

export interface ClientOptions {
  /** Base URL of the gateway, for example https://api.example.com */
  baseUrl: string;
  /** A tenant API key, or an OAuth access token. */
  apiKey: string;
  fetch?: typeof fetch;
}

export class GeneratedClient {
  constructor(private readonly options: ClientOptions) {}

  protected async request(
    method: string,
    path: string,
    body?: unknown,
    init?: RequestInit,
  ): Promise<Response> {
    const doFetch = this.options.fetch ?? fetch;
    const headers: Record<string, string> = {
      Authorization: `Bearer ${this.options.apiKey}`,
      ...(init?.headers as Record<string, string> | undefined),
    };
    if (body !== undefined) headers["Content-Type"] = "application/json";
    const response = await doFetch(this.options.baseUrl.replace(/\/$/, "") + path, {
      ...init,
      method,
      headers,
      body: body === undefined ? init?.body : JSON.stringify(body),
    });
    if (!response.ok) {
      let code = "internal";
      let message = response.statusText;
      try {
        const failure = await response.json();
        code = failure?.error?.code ?? code;
        message = failure?.error?.message ?? message;
      } catch {
        // A body that is not JSON keeps the status text.
      }
      throw new KilnError(response.status, code, message);
    }
    return response;
  }

  protected async json<T>(method: string, path: string, body?: unknown): Promise<T> {
    const response = await this.request(method, path, body);
    if (response.status === 204) return undefined as T;
    return (await response.json()) as T;
  }

  /** Report whether the host can run sandboxes. */
  health(): Promise<unknown> {
    return this.json("GET", "/v1/health");
  }

  /** List the registry credentials of this tenant. The tokens are never returned. */
  listRegistries(): Promise<unknown> {
    return this.json("GET", "/v1/registries");
  }

  /** Store the credential a template build uses to pull a private image for this tenant. */
  putRegistry(body: { host: string; username: string; token: string }): Promise<unknown> {
    return this.json("POST", "/v1/registries", body);
  }

  /** Remove one registry credential. */
  deleteRegistry(host: string): Promise<void> {
    return this.json("DELETE", `/v1/registries/${encodeURIComponent(String(host))}`);
  }

  /** List the sandboxes of this tenant. */
  listSandboxes(): Promise<unknown> {
    return this.json("GET", "/v1/sandboxes");
  }

  /** Create a sandbox by restoring a template snapshot. */
  createSandbox(body: { template: string; lifecycle: string; idle_seconds: number; ttl_seconds?: number; metadata?: Record<string, unknown>; secrets?: string[]; env?: Record<string, unknown> }): Promise<unknown> {
    return this.json("POST", "/v1/sandboxes", body);
  }

  /** Destroy a sandbox and retire its hostnames. */
  deleteSandbox(id: string): Promise<void> {
    return this.json("DELETE", `/v1/sandboxes/${encodeURIComponent(String(id))}`);
  }

  /** Read one sandbox with its published ports. */
  getSandbox(id: string): Promise<unknown> {
    return this.json("GET", `/v1/sandboxes/${encodeURIComponent(String(id))}`);
  }

  /** Replace the credential bindings of one sandbox. A request from the sandbox to a bound host leaves with the header that Kiln sets, and the value never enters the guest. A value is never returned. A rotation is a new call. A sleeping sandbox takes the change without a wake. A template built before credential bindings answers conflict and must be rebuilt. */
  setCredentials(id: string, body: { credentials: Record<string, unknown>[] }): Promise<unknown> {
    return this.json("PUT", `/v1/sandboxes/${encodeURIComponent(String(id))}/credentials`, body);
  }

  /** Replace the egress policy of one sandbox. Send egress_allow or observe. The answer comes after the host swaps the policy and cuts every connection the guest opened. A connection to a published port stays. A sleeping sandbox takes the change without a wake, and a later snapshot or fork carries it. */
  setEgress(id: string, body: { egress_allow?: string[]; observe?: boolean }): Promise<unknown> {
    return this.json("PUT", `/v1/sandboxes/${encodeURIComponent(String(id))}/egress`, body);
  }

  /** Read the egress record: one row per destination, with its decision, counts and bytes. A row names a host, or an address when the connection named no host. A fork or a restore starts with no rows. The rows stay for 7 days after a destroy. */
  egressLog(id: string): Promise<unknown> {
    return this.json("GET", `/v1/sandboxes/${encodeURIComponent(String(id))}/egress/log`);
  }

  /** Read the request log of the bound hosts of one sandbox: method, host, path with no query string, status, binding and time. It holds no header value and no body. It holds at most 10000 rows, and dropped counts the rest. The rows stay for 7 days after a destroy. */
  egressRequests(id: string): Promise<unknown> {
    return this.json("GET", `/v1/sandboxes/${encodeURIComponent(String(id))}/egress/requests`);
  }

  /** Run a command inside a sandbox. Accept text/event-stream to read the output as it runs. */
  exec(id: string, body: { cmd: string[]; cwd?: string; env?: Record<string, unknown>; timeout_seconds?: number }): Promise<unknown> {
    return this.json("POST", `/v1/sandboxes/${encodeURIComponent(String(id))}/exec`, body);
  }

  /** Snapshot a running sandbox and restore several copies of it. */
  fork(id: string, body: { count: number; allow_secret_fork?: boolean; credentials?: Record<string, unknown>[]; metadata?: Record<string, unknown> }): Promise<unknown> {
    return this.json("POST", `/v1/sandboxes/${encodeURIComponent(String(id))}/fork`, body);
  }

  /** Read what the guest kept of the start command output, how the start command ended, and the template build result. A sleeping sandbox wakes first. */
  logs(id: string): Promise<unknown> {
    return this.json("GET", `/v1/sandboxes/${encodeURIComponent(String(id))}/logs`);
  }

  /** Start a detached process and answer once it runs. It has no deadline, it outlives this request, and the sandbox does not sleep while it runs. */
  startProcess(id: string, body: { cmd: string[]; cwd?: string; env?: Record<string, unknown> }): Promise<unknown> {
    return this.json("POST", `/v1/sandboxes/${encodeURIComponent(String(id))}/processes`, body);
  }

  /** Send a signal to the process group of a detached process. A process that has ended takes no signal, and the call still succeeds. */
  signalProcess(id: string, pid: string, query?: { signal?: string }): Promise<void> {
    const search = new URLSearchParams();
    if (query?.signal !== undefined) search.set("signal", String(query.signal));
    const suffix = search.size > 0 ? `?${search}` : "";
    return this.json("DELETE", `/v1/sandboxes/${encodeURIComponent(String(id))}/processes/${encodeURIComponent(String(pid))}` + suffix);
  }

  /** Read the state of a detached process and its output after each offset. One answer holds at most 1 MiB of each stream, the guest keeps the last 8 MiB, and the answer names the next offsets and the bytes lost. */
  readProcess(id: string, pid: string, query?: { stdout_offset?: number; stderr_offset?: number }): Promise<unknown> {
    const search = new URLSearchParams();
    if (query?.stdout_offset !== undefined) search.set("stdout_offset", String(query.stdout_offset));
    if (query?.stderr_offset !== undefined) search.set("stderr_offset", String(query.stderr_offset));
    const suffix = search.size > 0 ? `?${search}` : "";
    return this.json("GET", `/v1/sandboxes/${encodeURIComponent(String(id))}/processes/${encodeURIComponent(String(pid))}` + suffix);
  }

  /** Serve a guest port on a public hostname. */
  publish(id: string, body: { port: number; visibility: string }): Promise<unknown> {
    return this.json("POST", `/v1/sandboxes/${encodeURIComponent(String(id))}/publish`, body);
  }

  /** Retire a published hostname. It answers 404 from then on. */
  retire(id: string, port: number): Promise<void> {
    return this.json("DELETE", `/v1/sandboxes/${encodeURIComponent(String(id))}/publish/${encodeURIComponent(String(port))}`);
  }

  /** Snapshot a sandbox. With stop the sandbox sleeps and keeps its hostnames. */
  snapshot(id: string, body: { stop?: boolean }): Promise<unknown> {
    return this.json("POST", `/v1/sandboxes/${encodeURIComponent(String(id))}/snapshot`, body);
  }

  /** List the snapshots of this tenant. */
  listSnapshots(): Promise<unknown> {
    return this.json("GET", "/v1/snapshots");
  }

  /** Delete a snapshot that has no live sandbox. */
  deleteSnapshot(id: string): Promise<void> {
    return this.json("DELETE", `/v1/snapshots/${encodeURIComponent(String(id))}`);
  }

  /** Read one snapshot. */
  getSnapshot(id: string): Promise<unknown> {
    return this.json("GET", `/v1/snapshots/${encodeURIComponent(String(id))}`);
  }

  /** Restore a snapshot into one or more new sandboxes. */
  restore(id: string, body: { count: number; allow_secret_fork?: boolean; credentials?: Record<string, unknown>[]; metadata?: Record<string, unknown> }): Promise<unknown> {
    return this.json("POST", `/v1/snapshots/${encodeURIComponent(String(id))}/restore`, body);
  }

  /** List the templates of this tenant. */
  listTemplates(): Promise<unknown> {
    return this.json("GET", "/v1/templates");
  }

  /** Build a template from an OCI image. The build runs after the answer. */
  createTemplate(body: { name: string; image: string; vcpus: number; memory_mb: number; disk_mb: number; egress_allow: string[]; setup?: string[]; ttl_seconds?: number; start?: string[]; port?: number; services?: Record<string, unknown>[]; seed?: string[]; user?: string; credential?: Record<string, unknown> }): Promise<unknown> {
    return this.json("POST", "/v1/templates", body);
  }

  /** Delete a template that has no live sandbox and no snapshot. */
  deleteTemplate(name: string): Promise<void> {
    return this.json("DELETE", `/v1/templates/${encodeURIComponent(String(name))}`);
  }

  /** Read one template. */
  getTemplate(name: string): Promise<unknown> {
    return this.json("GET", `/v1/templates/${encodeURIComponent(String(name))}`);
  }

  /** List the viewers of this tenant. */
  listViewers(): Promise<unknown> {
    return this.json("GET", "/v1/viewers");
  }

  /** Add a viewer of this tenant. A viewer opens team previews and nothing else. */
  createViewer(body: { email: string; password: string }): Promise<unknown> {
    return this.json("POST", "/v1/viewers", body);
  }

  /** Revoke one viewer. The viewer is disabled and its sessions end. */
  deleteViewer(id: string): Promise<void> {
    return this.json("DELETE", `/v1/viewers/${encodeURIComponent(String(id))}`);
  }
}
