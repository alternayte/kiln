import { useSession as useAuthSession } from "@alternayte/auth-all-client/react";
import { useQuery } from "@tanstack/react-query";

import { sessionStore } from "@/lib/auth";

/** Who the caller is, and what the caller may do, from the gateway. */
export interface Me {
  user_id: string;
  email: string;
  tenant_id?: string;
  tenant_name?: string;
  role?: string;
  is_operator: boolean;
}

/** The auth-all session: whether a person is signed in at all. */
export function useSession() {
  return useAuthSession(sessionStore);
}

/**
 * useMe reads the active tenant and the role. It is a separate call because
 * the tenant and the role live in the gateway's principal, not in the
 * auth-all session.
 */
export function useMe() {
  const { user } = useSession();
  return useQuery({
    queryKey: ["me", user?.id],
    enabled: Boolean(user),
    queryFn: async (): Promise<Me> => {
      const response = await fetch("/api/me", { credentials: "include" });
      if (!response.ok) throw new Error("the session names no tenant");
      return response.json();
    },
  });
}

/**
 * can asks the same permission the route asks. The server refuses anyway; a
 * hidden control is a convenience, not a check.
 */
export function useCan(permission: string): boolean {
  const { data } = useMe();
  const role = data?.role ?? "reader";
  if (role === "operator") return true;
  if (role === "editor") return !permission.startsWith("tenant:");
  return permission.endsWith(":read");
}

export function useIsOperator(): boolean {
  return useMe().data?.is_operator ?? false;
}
