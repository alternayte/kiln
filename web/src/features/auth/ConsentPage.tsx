import { useEffect, useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";

import { Button, Notice } from "@/components/ui";
import { AuthFrame } from "./AuthFrame";

interface ConsentRequest {
  clientName?: string;
  scopes?: string[];
}

// The OAuth provider serves the request and the decision under /api/auth.
// This screen only shows what the application asks for and posts the answer.
const PREFIX = "/api/auth";

export function ConsentPage() {
  const [params] = useSearchParams();
  const navigate = useNavigate();
  const requestId = params.get("request_id");
  const [request, setRequest] = useState<ConsentRequest | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    if (!requestId) {
      setError("This page opens from an application, and it carries no request.");
      return;
    }
    (async () => {
      const response = await fetch(
        `${PREFIX}/oauth2/request?request_id=${encodeURIComponent(requestId)}`,
        { credentials: "include" },
      );
      if (response.status === 401) {
        navigate(`/sign-in?request_id=${encodeURIComponent(requestId)}`, { replace: true });
        return;
      }
      if (!response.ok) {
        setError("This authorization request is unknown or spent. Start again from the application.");
        return;
      }
      setRequest(await response.json());
    })();
  }, [requestId, navigate]);

  async function decide(approve: boolean) {
    setBusy(true);
    setError("");
    const response = await fetch(`${PREFIX}/oauth2/decide`, {
      method: "POST",
      credentials: "include",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ requestId, approve }),
    });
    if (!response.ok) {
      const body = await response.json().catch(() => ({}));
      setError(body?.error?.message ?? "The decision failed. Start again from the application.");
      setBusy(false);
      return;
    }
    const result = await response.json();
    window.location.href = result.redirectTo;
  }

  return (
    <AuthFrame
      title={request?.clientName ? `${request.clientName} asks for access` : "Authorize"}
      subtitle={
        request ? "It will act on your sandboxes with the access you allow here." : undefined
      }
    >
      {request?.scopes?.length ? (
        <ul className="space-y-1.5 rounded-lg border border-edge bg-raised p-4">
          {request.scopes.map((scope) => (
            <li key={scope} className="font-mono text-xs text-muted">
              {scope}
            </li>
          ))}
        </ul>
      ) : null}
      <Notice>{error}</Notice>
      {request ? (
        <div className="flex gap-3">
          <Button variant="primary" className="flex-1" disabled={busy} onClick={() => decide(true)}>
            Allow
          </Button>
          <Button className="flex-1" disabled={busy} onClick={() => decide(false)}>
            Refuse
          </Button>
        </div>
      ) : null}
    </AuthFrame>
  );
}
