import { AuthAllClient } from "@alternayte/auth-all-client";
import { createSessionStore } from "@alternayte/auth-all-client/react";

// The UI is served by the gateway, so the session cookie is same-origin and
// the browser holds no token. The client's own paths match the gateway's
// auth prefix, so nothing here restates a route.
export const authClient = new AuthAllClient({ credentials: "include" });

export const sessionStore = createSessionStore(authClient);

/** The message to show a person for a failed call. */
export function errorText(error: unknown): string {
  const body = (error as { error?: { message?: string } } | undefined)?.error;
  if (body?.message) return body.message;
  if (error instanceof Error) return error.message;
  return "The call failed.";
}
