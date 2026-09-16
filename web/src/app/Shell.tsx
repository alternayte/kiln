import { NavLink, Outlet, useNavigate } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Boxes,
  ChevronDown,
  KeyRound,
  Layers,
  LogOut,
  Monitor,
  Users,
  Building2,
} from "lucide-react";

import { authClient, sessionStore } from "@/lib/auth";
import { cn } from "@/lib/cn";
import { useCan, useIsOperator, useMe, useSession } from "./session";

const NAV = [
  { to: "/sandboxes", label: "Sandboxes", icon: Boxes, permission: "sandbox:read" },
  { to: "/templates", label: "Templates", icon: Layers, permission: "template:read" },
  { to: "/keys", label: "API keys", icon: KeyRound, permission: "key:write" },
  { to: "/members", label: "Members", icon: Users, permission: "member:write" },
  { to: "/viewers", label: "Viewers", icon: Monitor, permission: "viewer:write" },
  { to: "/tenants", label: "Tenants", icon: Building2, permission: "tenant:write" },
];

export function Shell() {
  return (
    // The column is the window, so a screen that wants the rest of the height
    // can take it and nothing outside it scrolls.
    <div className="flex h-screen flex-col">
      <TopBar />
      <div className="mx-auto flex w-full min-h-0 max-w-7xl flex-1 gap-8 px-4 py-6 sm:px-6">
        <Sidebar />
        <main className="flex min-h-0 min-w-0 flex-1 flex-col gap-6 overflow-y-auto">
          <Outlet />
        </main>
      </div>
    </div>
  );
}

function Sidebar() {
  // Hooks cannot run in a loop, so every permission is read once here.
  const allowed: Record<string, boolean> = {
    "sandbox:read": useCan("sandbox:read"),
    "template:read": useCan("template:read"),
    "key:write": useCan("key:write"),
    "member:write": useCan("member:write"),
    "viewer:write": useCan("viewer:write"),
    "tenant:write": useIsOperator(),
  };
  return (
    <nav className="hidden w-48 shrink-0 space-y-0.5 sm:block">
      {NAV.filter((item) => allowed[item.permission]).map((item) => (
        <NavLink
          key={item.to}
          to={item.to}
          className={({ isActive }) =>
            cn(
              "flex items-center gap-2.5 rounded-md px-3 py-2 text-sm transition-colors",
              isActive ? "bg-raised text-ink" : "text-muted hover:text-ink",
            )
          }
        >
          <item.icon className="size-4" aria-hidden />
          {item.label}
        </NavLink>
      ))}
    </nav>
  );
}

function TopBar() {
  const { user } = useSession();
  const me = useMe();
  const navigate = useNavigate();
  const queries = useQueryClient();

  async function out() {
    await authClient.signOut();
    await sessionStore.refresh();
    queries.clear();
    navigate("/sign-in", { replace: true });
  }

  return (
    <header className="border-b border-edge">
      <div className="mx-auto flex w-full max-w-7xl items-center gap-4 px-4 py-3 sm:px-6">
        <span className="font-mono text-sm font-semibold tracking-tight">
          <span className="text-ember">▲</span> kiln
        </span>
        <TenantSwitcher active={me.data?.tenant_id} name={me.data?.tenant_name} />
        <div className="ml-auto flex items-center gap-3">
          <span className="hidden text-xs text-muted sm:inline">{user?.email}</span>
          {me.data?.role ? (
            <span className="rounded border border-edge px-1.5 py-0.5 font-mono text-[11px] text-muted">
              {me.data.role}
            </span>
          ) : null}
          <button
            onClick={out}
            className="text-muted transition-colors hover:text-ink"
            aria-label="Sign out"
          >
            <LogOut className="size-4" />
          </button>
        </div>
      </div>
    </header>
  );
}

// The switcher shows only when the person belongs to more than one tenant.
function TenantSwitcher({ active, name }: { active?: string; name?: string }) {
  const queries = useQueryClient();
  const tenants = useQuery({
    queryKey: ["organizations"],
    queryFn: () => authClient.organizations.list(),
  });
  const rows = tenants.data?.organizations ?? [];
  if (rows.length < 2) {
    return name ? <span className="text-sm text-muted">{name}</span> : null;
  }
  return (
    <label className="relative flex items-center">
      <select
        value={active ?? ""}
        onChange={async (event) => {
          await authClient.organizations.setActive(event.target.value);
          queries.clear();
        }}
        className="appearance-none rounded-md border border-edge bg-raised py-1 pr-7 pl-2.5 text-sm"
        aria-label="Active tenant"
      >
        {rows.map((tenant) => (
          <option key={tenant.id} value={tenant.id}>
            {tenant.name}
          </option>
        ))}
      </select>
      <ChevronDown className="pointer-events-none absolute right-2 size-3.5 text-muted" aria-hidden />
    </label>
  );
}
