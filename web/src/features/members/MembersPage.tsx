import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { authClient, errorText } from "@/lib/auth";
import { useMe } from "@/app/session";
import { Button, Empty, Field, Mono, Notice, Panel, Table } from "@/components/ui";

const ROLES = ["reader", "editor", "operator"];

export function MembersPage() {
  const queries = useQueryClient();
  const me = useMe();
  const tenant = me.data?.tenant_id ?? "";
  const [email, setEmail] = useState("");
  const [role, setRole] = useState("editor");
  // No mail is sent. The inviter passes this link on, the way the plaintext
  // of an API key is passed on.
  const [link, setLink] = useState("");

  const members = useQuery({
    queryKey: ["members", tenant],
    enabled: Boolean(tenant),
    queryFn: () => authClient.organizations.listMembers(tenant),
  });
  const invitations = useQuery({
    queryKey: ["invitations", tenant],
    enabled: Boolean(tenant),
    queryFn: () => authClient.organizations.listInvitations(tenant),
  });

  const invite = useMutation({
    mutationFn: () => authClient.organizations.invite(tenant, { email, role }),
    onSuccess: (answer) => {
      const url = new URL("/accept-invitation", window.location.origin);
      url.searchParams.set("token", answer.token);
      url.searchParams.set("email", email);
      setLink(url.toString());
      setEmail("");
      queries.invalidateQueries({ queryKey: ["invitations", tenant] });
    },
  });

  const setRoleOf = useMutation({
    mutationFn: ({ userId, next }: { userId: string; next: string }) =>
      authClient.organizations.setMember(tenant, userId, { role: next }),
    onSuccess: () => queries.invalidateQueries({ queryKey: ["members", tenant] }),
  });

  const remove = useMutation({
    mutationFn: (userId: string) => authClient.organizations.removeMember(tenant, userId),
    onSuccess: () => queries.invalidateQueries({ queryKey: ["members", tenant] }),
  });

  const pending = (invitations.data?.invitations ?? []).filter((row) => row.status === "pending");

  return (
    <>
      <h1 className="text-lg font-semibold tracking-tight">Members</h1>

      {link ? (
        <Panel title="Send this link to the person you invited">
          <div className="space-y-3 p-4">
            <p className="text-sm text-muted">
              Kiln sends no mail. The link creates the sign-in and joins this tenant. It expires.
            </p>
            <div className="flex items-center gap-2">
              <code className="flex-1 overflow-x-auto rounded-md border border-ember/40 bg-ground px-3 py-2 font-mono text-xs text-ember">
                {link}
              </code>
              <Button onClick={() => navigator.clipboard.writeText(link)}>Copy</Button>
              <Button variant="ghost" onClick={() => setLink("")}>
                Done
              </Button>
            </div>
          </div>
        </Panel>
      ) : null}

      <Panel
        title="People in this tenant"
        actions={
          <div className="flex items-end gap-2">
            <Field
              label="Email"
              type="email"
              value={email}
              onChange={(event) => setEmail(event.target.value)}
              className="w-56"
            />
            <select
              value={role}
              onChange={(event) => setRole(event.target.value)}
              aria-label="Role"
              className="rounded-md border border-edge bg-ground px-2.5 py-2 text-sm"
            >
              {ROLES.map((name) => (
                <option key={name} value={name}>
                  {name}
                </option>
              ))}
            </select>
            <Button variant="primary" disabled={!email || invite.isPending} onClick={() => invite.mutate()}>
              Invite
            </Button>
          </div>
        }
      >
        {invite.error ? <div className="p-3"><Notice>{errorText(invite.error)}</Notice></div> : null}
        {(members.data?.members ?? []).length === 0 ? (
          <Empty>{members.isPending ? "Reading the members." : "No member yet."}</Empty>
        ) : (
          <Table head={["Email", "Role", "Joined", ""]}>
            {(members.data?.members ?? []).map((member) => (
              <tr key={member.userId}>
                <td className="px-4 py-2.5"><Mono>{member.userId}</Mono></td>
                <td className="px-4 py-2.5">
                  <select
                    value={member.role}
                    onChange={(event) =>
                      setRoleOf.mutate({ userId: member.userId, next: event.target.value })
                    }
                    aria-label={`Role of ${member.userId}`}
                    className="rounded-md border border-edge bg-ground px-2 py-1 text-sm"
                  >
                    {ROLES.map((name) => (
                      <option key={name} value={name}>
                        {name}
                      </option>
                    ))}
                  </select>
                </td>
                <td className="px-4 py-2.5 text-muted">
                  {member.joinedAt ? new Date(member.joinedAt).toLocaleDateString() : "—"}
                </td>
                <td className="px-4 py-2.5 text-right">
                  <Button
                    variant="ghost"
                    onClick={() => {
                      if (window.confirm("Remove this member from the tenant?")) {
                        remove.mutate(member.userId);
                      }
                    }}
                  >
                    Remove
                  </Button>
                </td>
              </tr>
            ))}
          </Table>
        )}
      </Panel>

      {pending.length > 0 ? (
        <Panel title="Invitations not yet accepted">
          <Table head={["Email", "Role", "Expires", ""]}>
            {pending.map((row) => (
              <tr key={row.id}>
                <td className="px-4 py-2.5">{row.email}</td>
                <td className="px-4 py-2.5 text-muted">{row.role}</td>
                <td className="px-4 py-2.5 text-muted">
                  {row.expiresAt ? new Date(row.expiresAt).toLocaleString() : "—"}
                </td>
                <td className="px-4 py-2.5 text-right">
                  <Button
                    variant="ghost"
                    onClick={async () => {
                      await authClient.organizations.revokeInvitation(tenant, row.id);
                      queries.invalidateQueries({ queryKey: ["invitations", tenant] });
                    }}
                  >
                    Revoke
                  </Button>
                </td>
              </tr>
            ))}
          </Table>
        </Panel>
      ) : null}
    </>
  );
}
