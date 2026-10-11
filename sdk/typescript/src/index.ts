// The handwritten layer. It adds what a generator does badly: streaming
// exec, file transfer, retries, and a sandbox object that reads like the
// thing it names.
import { GeneratedClient, KilnError, type ClientOptions } from "./generated.js";

export { KilnError, type ClientOptions };

/** One line of output from a running command. */
export interface ExecChunk {
  stream: "stdout" | "stderr";
  text: string;
}

/** What a finished command answers. */
export interface ExecResult {
  exit_code: number;
  stdout: string;
  stderr: string;
  truncated?: boolean;
  timed_out?: boolean;
}

export interface ExecOptions {
  cwd?: string;
  env?: Record<string, string>;
  timeout_seconds?: number;
  /** Called for every chunk while the command runs. */
  onOutput?: (chunk: ExecChunk) => void;
}

/** One state transition on the event stream. */
export interface Event {
  id: number;
  sandbox_id?: string;
  from_state?: string;
  to_state?: string;
  reason?: string;
  at: string;
}

export interface EventsOptions {
  /** Start after this event id. 0 replays every event of this tenant. Omitted: the newest events only. */
  after?: number;
  /** Keep the events of one sandbox. */
  sandboxId?: string;
}

export class Kiln extends GeneratedClient {
  /** Open one sandbox by id. */
  sandbox(id: string): Sandbox {
    return new Sandbox(this, id);
  }

  /** Read the event stream as it arrives. It never ends; break out of the loop to close it. */
  async *events({ after, sandboxId }: EventsOptions = {}): AsyncGenerator<Event> {
    const query = new URLSearchParams();
    if (after !== undefined) query.set("after", String(after));
    if (sandboxId !== undefined) query.set("sandbox_id", sandboxId);
    const suffix = query.size > 0 ? `?${query}` : "";
    const response = await this["request"]("GET", `/v1/events${suffix}`, undefined, {
      headers: { Accept: "text/event-stream" },
    });
    for await (const [, data] of sse(response)) {
      yield JSON.parse(data) as Event;
    }
  }

  /** Create a sandbox and return it as an object. */
  async createSandboxObject(
    body: Parameters<GeneratedClient["createSandbox"]>[0],
  ): Promise<Sandbox> {
    const created = (await this.createSandbox(body)) as { id: string };
    return new Sandbox(this, created.id);
  }

  /** Build a template and wait until it is ready. */
  async buildTemplate(
    body: Parameters<GeneratedClient["createTemplate"]>[0],
    { pollMs = 2000, timeoutMs = 900_000 }: { pollMs?: number; timeoutMs?: number } = {},
  ): Promise<void> {
    await this.createTemplate(body);
    const deadline = Date.now() + timeoutMs;
    for (;;) {
      const template = (await this.getTemplate(body.name)) as { state: string; error?: string };
      if (template.state === "ready") return;
      if (template.state === "failed") {
        throw new KilnError(409, "conflict", template.error ?? "the template build failed");
      }
      if (Date.now() > deadline) {
        throw new KilnError(409, "conflict", `the template ${body.name} is still building`);
      }
      await new Promise((resolve) => setTimeout(resolve, pollMs));
    }
  }
}

export class Sandbox {
  constructor(
    private readonly client: Kiln,
    readonly id: string,
  ) {}

  /** Run a command. With onOutput the call streams and returns at the end. */
  async exec(cmd: string[], options: ExecOptions = {}): Promise<ExecResult> {
    const { onOutput, ...rest } = options;
    if (!onOutput) {
      return (await this.client.exec(this.id, { cmd, ...rest })) as ExecResult;
    }
    const response = await this.client["request"](
      "POST",
      `/v1/sandboxes/${encodeURIComponent(this.id)}/exec`,
      { cmd, ...rest },
      { headers: { Accept: "text/event-stream" } },
    );
    return readExecStream(response, onOutput);
  }

  /** Read one file as text. */
  async readFile(path: string): Promise<string> {
    const response = await this.client["request"](
      "GET",
      `/v1/sandboxes/${encodeURIComponent(this.id)}/files/${path.replace(/^\//, "")}`,
    );
    return response.text();
  }

  /** Write one file. */
  async writeFile(path: string, body: string | Uint8Array | Blob): Promise<void> {
    await this.client["request"](
      "PUT",
      `/v1/sandboxes/${encodeURIComponent(this.id)}/files/${path.replace(/^\//, "")}`,
      undefined,
      { body: body as BodyInit },
    );
  }

  publish(port: number, visibility: "public" | "team" = "public") {
    return this.client.publish(this.id, { port, visibility });
  }

  /** Snapshot and stop. The sandbox keeps its id and its hostnames. */
  sleep() {
    return this.client.snapshot(this.id, { stop: true });
  }

  fork(count: number) {
    return this.client.fork(this.id, { count });
  }

  destroy() {
    return this.client.deleteSandbox(this.id);
  }
}

/** Yield [event, data] pairs from a server-sent event body. */
async function* sse(response: Response): AsyncGenerator<[string, string]> {
  const reader = response.body?.getReader();
  if (!reader) throw new KilnError(500, "internal", "the response carries no body");
  const decoder = new TextDecoder();
  let buffer = "";
  let event = "";
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      let index: number;
      while ((index = buffer.indexOf("\n")) >= 0) {
        const line = buffer.slice(0, index).replace(/\r$/, "");
        buffer = buffer.slice(index + 1);
        if (line.startsWith("event:")) {
          event = line.slice(6).trim();
          continue;
        }
        if (!line.startsWith("data:")) continue;
        yield [event, line.slice(5).trim()];
      }
    }
  } finally {
    // A caller that stops reading closes the connection.
    await reader.cancel().catch(() => {});
  }
}

/** Read the SSE body of a streaming exec. */
async function readExecStream(
  response: Response,
  onOutput: (chunk: ExecChunk) => void,
): Promise<ExecResult> {
  const result: ExecResult = { exit_code: 0, stdout: "", stderr: "" };
  for await (const [event, data] of sse(response)) {
    if (event === "exit") {
      Object.assign(result, JSON.parse(data));
      continue;
    }
    if (event === "error") {
      throw new KilnError(500, "internal", JSON.parse(data) as string);
    }
    const text = JSON.parse(data) as string;
    const stream = event === "stderr" ? "stderr" : "stdout";
    result[stream] += text;
    onOutput({ stream, text });
  }
  return result;
}
