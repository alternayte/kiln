import { useState, type FormEvent } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";

import { authClient, errorText, sessionStore } from "@/lib/auth";
import { Button, Field, Notice } from "@/components/ui";
import { AuthFrame } from "./AuthFrame";

// An invitation is the only route in. The sign-in is created and the
// invitation accepted in one submit, because auth-all accepts an invitation
// only for a signed-in person.
export function AcceptInvitationPage() {
  const [params] = useSearchParams();
  const navigate = useNavigate();
  const token = params.get("token") ?? "";
  const [email, setEmail] = useState(params.get("email") ?? "");
  const [name, setName] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  async function submit(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    setError("");
    try {
      // A person who already signs in here joins instead of registering, so
      // a second invitation to the same address still works.
      try {
        await authClient.signUp.email({ email, password, name: name || email });
      } catch {
        await authClient.signIn.email({ email, password });
      }
      await authClient.organizations.acceptInvitation({ token });
      await sessionStore.refresh();
      navigate("/sandboxes", { replace: true });
    } catch (failure) {
      setError(errorText(failure));
    } finally {
      setBusy(false);
    }
  }

  if (!token) {
    return (
      <AuthFrame title="This link carries no invitation">
        <p className="text-sm text-muted">
          Ask the person who invited you for the link again. It expires.
        </p>
      </AuthFrame>
    );
  }

  return (
    <AuthFrame title="Join Kiln" subtitle="Set a password and you are in.">
      <form onSubmit={submit} className="space-y-4">
        <Field
          label="Email"
          type="email"
          autoComplete="username"
          required
          value={email}
          onChange={(event) => setEmail(event.target.value)}
          hint="Use the address the invitation was sent to."
        />
        <Field
          label="Name"
          autoComplete="name"
          value={name}
          onChange={(event) => setName(event.target.value)}
        />
        <Field
          label="Password"
          type="password"
          autoComplete="new-password"
          required
          value={password}
          onChange={(event) => setPassword(event.target.value)}
        />
        <Notice>{error}</Notice>
        <Button type="submit" variant="primary" className="w-full" disabled={busy}>
          {busy ? "Joining" : "Join"}
        </Button>
      </form>
    </AuthFrame>
  );
}
