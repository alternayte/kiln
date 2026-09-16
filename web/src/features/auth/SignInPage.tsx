import { useState, type FormEvent } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";

import { authClient, errorText, sessionStore } from "@/lib/auth";
import { Button, Field, Notice } from "@/components/ui";
import { AuthFrame } from "./AuthFrame";

// This screen replaces the gateway's old server-rendered page. The OAuth
// provider sends a browser here with a request_id, and the consent screen
// takes over once the person is signed in.
export function SignInPage() {
  const [params] = useSearchParams();
  const navigate = useNavigate();
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const requestId = params.get("request_id");
  const next = params.get("next");

  async function submit(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    setError("");
    try {
      await authClient.signIn.email({ email, password });
      await sessionStore.refresh();
      if (requestId) {
        navigate(`/consent?request_id=${encodeURIComponent(requestId)}`, { replace: true });
        return;
      }
      navigate(next && next.startsWith("/") ? next : "/sandboxes", { replace: true });
    } catch (failure) {
      setError(errorText(failure) || "That email and password do not match.");
    } finally {
      setBusy(false);
    }
  }

  return (
    <AuthFrame
      title="Sign in to Kiln"
      subtitle={
        requestId
          ? "An application asked for access to your sandboxes."
          : "Run isolated Linux microVMs."
      }
    >
      <form onSubmit={submit} className="space-y-4">
        <Field
          label="Email"
          type="email"
          name="email"
          autoComplete="username"
          required
          value={email}
          onChange={(event) => setEmail(event.target.value)}
        />
        <Field
          label="Password"
          type="password"
          name="password"
          autoComplete="current-password"
          required
          value={password}
          onChange={(event) => setPassword(event.target.value)}
        />
        <Notice>{error}</Notice>
        <Button type="submit" variant="primary" className="w-full" disabled={busy}>
          {busy ? "Signing in" : "Sign in"}
        </Button>
        <p className="text-center text-xs text-muted">
          Kiln is invite only. An operator sends the invitation that lets you in.
        </p>
      </form>
    </AuthFrame>
  );
}
