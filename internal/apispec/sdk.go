package apispec

import (
	"fmt"
	"sort"
	"strings"
)

// TypeScript returns the generated client. It covers every tenant operation,
// and the handwritten layer in sdk/typescript/src/kiln.ts builds on it.
func TypeScript() string {
	var b strings.Builder
	b.WriteString("// Generated from internal/apispec. Do not edit.\n")
	b.WriteString("// Run: just sdk\n\n")
	b.WriteString(`export class KilnError extends Error {
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
      Authorization: ` + "`Bearer ${this.options.apiKey}`" + `,
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
`)
	for _, op := range Operations() {
		if op.Operator || op.Terminal {
			continue
		}
		b.WriteString("\n  /** " + op.Summary + " */\n")
		fmt.Fprintf(&b, "  %s(%s): Promise<%s> {\n", op.ID, tsArgs(op), tsReturn(op))
		fmt.Fprintf(&b, "    return this.json(%q, %s%s);\n", op.Method, tsPath(op), tsBodyArg(op))
		b.WriteString("  }\n")
	}
	b.WriteString("}\n")
	return b.String()
}

// Python returns the generated client, for the handwritten layer in
// sdk/python/kiln/__init__.py.
func Python() string {
	var b strings.Builder
	b.WriteString("\"\"\"Generated from internal/apispec. Do not edit.\n\nRun: just sdk\n\"\"\"\n\n")
	b.WriteString(`from __future__ import annotations

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
`)
	for _, op := range Operations() {
		if op.Operator || op.Terminal {
			continue
		}
		fmt.Fprintf(&b, "\n    def %s(self%s) -> Any:\n", snake(op.ID), pyArgs(op))
		fmt.Fprintf(&b, "        \"\"\"%s\"\"\"\n", op.Summary)
		if len(op.Body) > 0 {
			b.WriteString("        body = {k: v for k, v in {\n")
			for _, f := range op.Body {
				fmt.Fprintf(&b, "            %q: %s,\n", f.Name, snake(f.Name))
			}
			b.WriteString("        }.items() if v is not None}\n")
			fmt.Fprintf(&b, "        return self._json(%q, %s, body)\n", op.Method, pyPath(op))
			continue
		}
		fmt.Fprintf(&b, "        return self._json(%q, %s)\n", op.Method, pyPath(op))
	}
	return b.String()
}

func tsArgs(op Operation) string {
	var args []string
	for _, p := range op.Params {
		args = append(args, fmt.Sprintf("%s: %s", p.Name, tsType(p)))
	}
	if len(op.Body) > 0 {
		var fields []string
		sorted := append([]Field(nil), op.Body...)
		sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Required && !sorted[j].Required })
		for _, f := range sorted {
			optional := "?"
			if f.Required {
				optional = ""
			}
			fields = append(fields, fmt.Sprintf("%s%s: %s", f.Name, optional, tsType(f)))
		}
		args = append(args, "body: { "+strings.Join(fields, "; ")+" }")
	}
	return strings.Join(args, ", ")
}

func tsBodyArg(op Operation) string {
	if len(op.Body) > 0 {
		return ", body"
	}
	return ""
}

func tsReturn(op Operation) string {
	if op.Returns == "" {
		return "void"
	}
	return "unknown"
}

func tsPath(op Operation) string {
	path := op.Path
	if len(op.Params) == 0 {
		return fmt.Sprintf("%q", path)
	}
	for _, p := range op.Params {
		value := "${encodeURIComponent(String(" + p.Name + "))}"
		path = strings.ReplaceAll(path, "{"+p.Name+"...}", value)
		path = strings.ReplaceAll(path, "{"+p.Name+"}", value)
	}
	return "`" + path + "`"
}

func tsType(f Field) string {
	switch f.Type {
	case "integer":
		return "number"
	case "boolean":
		return "boolean"
	case "array":
		return tsType(Field{Type: f.Items}) + "[]"
	case "object":
		return "Record<string, unknown>"
	default:
		return "string"
	}
}

func pyArgs(op Operation) string {
	var args []string
	for _, p := range op.Params {
		args = append(args, fmt.Sprintf(", %s: %s", snake(p.Name), pyType(p)))
	}
	required := ""
	optional := ""
	for _, f := range op.Body {
		if f.Required {
			required += fmt.Sprintf(", %s: %s", snake(f.Name), pyType(f))
			continue
		}
		optional += fmt.Sprintf(", %s: Optional[%s] = None", snake(f.Name), pyType(f))
	}
	return strings.Join(args, "") + required + optional
}

func pyPath(op Operation) string {
	path := op.Path
	if len(op.Params) == 0 {
		return fmt.Sprintf("%q", path)
	}
	for _, p := range op.Params {
		value := "{" + snake(p.Name) + "}"
		path = strings.ReplaceAll(path, "{"+p.Name+"...}", value)
		path = strings.ReplaceAll(path, "{"+p.Name+"}", value)
	}
	return "f" + fmt.Sprintf("%q", path)
}

func pyType(f Field) string {
	switch f.Type {
	case "integer":
		return "int"
	case "boolean":
		return "bool"
	case "array":
		return "list[" + pyType(Field{Type: f.Items}) + "]"
	case "object":
		return "dict[str, Any]"
	default:
		return "str"
	}
}

// snake turns an operation id or a field name into snake case.
func snake(name string) string {
	var b strings.Builder
	for i, r := range name {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r + 32)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
