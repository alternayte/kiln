import { useEffect } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";

import { authClient, errorText } from "@/lib/auth";
import { Button, Notice } from "@/components/ui";
import { AuthFrame } from "./AuthFrame";

/**
 * ChooseTenant stands between signing in and the app when the session names
 * no active tenant. Without it every call answers 403 and the screens sit
 * empty with nothing to act on.
 *
 * One membership needs no question, so it activates and moves on.
 */
export function ChooseTenantPage() {
  const queries = useQueryClient();
  const tenants = useQuery({
    queryKey: ["organizations"],
    queryFn: () => authClient.organizations.list(),
  });
  const rows = tenants.data?.organizations ?? [];

  async function activate(id: string) {
    await authClient.organizations.setActive(id);
    queries.clear();
  }

  useEffect(() => {
    if (rows.length === 1) void activate(rows[0].id);
    // activate is stable enough here: it closes over nothing that changes.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [rows.length]);

  if (tenants.isPending) {
    return <AuthFrame title="Reading your tenants" />;
  }
  if (tenants.isError) {
    return (
      <AuthFrame title="Your tenants could not be read">
        <Notice>{errorText(tenants.error)}</Notice>
      </AuthFrame>
    );
  }
  if (rows.length === 0) {
    return (
      <AuthFrame
        title="You belong to no tenant"
        subtitle="An operator invites you to one. Until then there is nothing here to act on."
      />
    );
  }
  return (
    <AuthFrame title="Choose a tenant" subtitle="Everything you do belongs to one tenant.">
      <div className="space-y-2">
        {rows.map((tenant) => (
          <Button key={tenant.id} className="w-full" onClick={() => activate(tenant.id)}>
            {tenant.name}
          </Button>
        ))}
      </div>
    </AuthFrame>
  );
}
