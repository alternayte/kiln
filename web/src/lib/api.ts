import { client } from "@/api/client.gen";

// The UI is served by the gateway, so every call is same-origin and the
// session cookie authenticates it. The browser holds no key: a credential
// that creates sandboxes must never reach front-end code.
client.setConfig({
  baseUrl: "",
  credentials: "include",
});

/** The shape every API error arrives in. */
export interface ApiError {
  error?: { code?: string; message?: string };
}

/** The message to show a person for a failed call. */
export function errorText(error: unknown): string {
  const body = (error as { error?: ApiError["error"] } | undefined)?.error;
  if (body?.message) return body.message;
  if (error instanceof Error) return error.message;
  return "The call failed.";
}
