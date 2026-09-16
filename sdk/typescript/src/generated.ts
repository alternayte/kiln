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

  /** Read the event stream of this tenant. */
  events(): Promise<unknown> {
    return this.json("GET", "/v1/events");
  }

  /** Report whether the host can run sandboxes. */
  health(): Promise<unknown> {
    return this.json("GET", "/v1/health");
  }

  /** List the sandboxes of this tenant. */
  listSandboxes(): Promise<unknown> {
    return this.json("GET", "/v1/sandboxes");
  }

  /** Create a sandbox by restoring a template snapshot. */
  createSandbox(body: { template: string; lifecycle?: string; ttl_seconds?: number; idle_seconds?: number; metadata?: Record<string, unknown>; secrets?: string[] }): Promise<unknown> {
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

  /** Run a command inside a sandbox. Accept text/event-stream to read the output as it runs. */
  exec(id: string, body: { cmd: string[]; cwd?: string; env?: Record<string, unknown>; timeout_seconds?: number }): Promise<unknown> {
    return this.json("POST", `/v1/sandboxes/${encodeURIComponent(String(id))}/exec`, body);
  }

  /** Read one file out of a sandbox. */
  readFile(id: string, path: string): Promise<unknown> {
    return this.json("GET", `/v1/sandboxes/${encodeURIComponent(String(id))}/files/${encodeURIComponent(String(path))}`);
  }

  /** Write one file into a sandbox. */
  writeFile(id: string, path: string): Promise<void> {
    return this.json("PUT", `/v1/sandboxes/${encodeURIComponent(String(id))}/files/${encodeURIComponent(String(path))}`);
  }

  /** Snapshot a running sandbox and restore several copies of it. */
  fork(id: string, body: { count: number; allow_secret_fork?: boolean }): Promise<unknown> {
    return this.json("POST", `/v1/sandboxes/${encodeURIComponent(String(id))}/fork`, body);
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
  restore(id: string, body: { count: number; allow_secret_fork?: boolean }): Promise<unknown> {
    return this.json("POST", `/v1/snapshots/${encodeURIComponent(String(id))}/restore`, body);
  }

  /** List the templates of this tenant. */
  listTemplates(): Promise<unknown> {
    return this.json("GET", "/v1/templates");
  }

  /** Build a template from an OCI image. The build runs after the answer. */
  createTemplate(body: { name: string; image: string; egress_allow: string[]; vcpus?: number; memory_mb?: number; disk_mb?: number; setup?: string[] }): Promise<unknown> {
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
}
