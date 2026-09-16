import { Navigate, Outlet, Route, Routes } from "react-router-dom";

import { useSession } from "./session";
import { Shell } from "./Shell";
import { SignInPage } from "@/features/auth/SignInPage";
import { ConsentPage } from "@/features/auth/ConsentPage";
import { AcceptInvitationPage } from "@/features/auth/AcceptInvitationPage";
import { SandboxesPage } from "@/features/sandboxes/SandboxesPage";
import {
  SandboxOverviewTab,
  SandboxPage,
  SandboxTerminalTab,
} from "@/features/sandboxes/SandboxPage";
import { TerminalPage } from "@/features/sandboxes/TerminalPage";
import { TemplatesPage } from "@/features/templates/TemplatesPage";
import { KeysPage } from "@/features/keys/KeysPage";
import { MembersPage } from "@/features/members/MembersPage";
import { ViewersPage } from "@/features/viewers/ViewersPage";
import { TenantsPage } from "@/features/tenants/TenantsPage";

export function App() {
  return (
    <Routes>
      <Route path="/sign-in" element={<SignInPage />} />
      <Route path="/consent" element={<ConsentPage />} />
      <Route path="/accept-invitation" element={<AcceptInvitationPage />} />
      <Route element={<RequireSession />}>
        <Route element={<Shell />}>
          <Route index element={<Navigate to="/sandboxes" replace />} />
          <Route path="/sandboxes" element={<SandboxesPage />} />
          <Route path="/sandboxes/:id" element={<SandboxPage />}>
            <Route index element={<SandboxOverviewTab />} />
            <Route path="terminal" element={<SandboxTerminalTab />} />
          </Route>
          <Route path="/templates" element={<TemplatesPage />} />
          <Route path="/keys" element={<KeysPage />} />
          <Route path="/members" element={<MembersPage />} />
          <Route path="/viewers" element={<ViewersPage />} />
          <Route path="/tenants" element={<TenantsPage />} />
        </Route>
        {/* The shell alone, filling the window, outside the app shell. */}
        <Route path="/terminal/:id" element={<TerminalPage />} />
      </Route>
      <Route path="*" element={<Navigate to="/sandboxes" replace />} />
    </Routes>
  );
}

// A visitor lands on the sign-in screen carrying the path they asked for, so
// the link they followed still works after they sign in.
function RequireSession() {
  const { user, isPending } = useSession();
  if (isPending) {
    return <p className="p-8 text-sm text-muted">Reading the session.</p>;
  }
  if (!user) {
    const next = window.location.pathname + window.location.search;
    return <Navigate to={`/sign-in?next=${encodeURIComponent(next)}`} replace />;
  }
  return <Outlet />;
}
