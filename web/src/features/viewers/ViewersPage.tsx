import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { createViewer, deleteViewer, listViewers } from "@/api";
import { errorText } from "@/lib/auth";
import { useCan } from "@/app/session";
import { Button, Empty, Field, Notice, Panel, Table } from "@/components/ui";

// A viewer is not a member. It opens the team previews of this tenant and
// reaches no sandbox, no template and no key, so it lives on its own screen.
export function ViewersPage() {
  const queries = useQueryClient();
  const can = useCan("viewer:write");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");

  const viewers = useQuery({
    queryKey: ["viewers"],
    queryFn: async () => (await listViewers({ throwOnError: true })).data,
  });

  const add = useMutation({
    mutationFn: async () =>
      (await createViewer({ body: { email, password }, throwOnError: true })).data,
    onSuccess: () => {
      setEmail("");
      setPassword("");
      queries.invalidateQueries({ queryKey: ["viewers"] });
    },
  });

  const revoke = useMutation({
    mutationFn: async (id: string) =>
      (await deleteViewer({ path: { id }, throwOnError: true })).data,
    onSuccess: () => queries.invalidateQueries({ queryKey: ["viewers"] }),
  });

  const rows = viewers.data ?? [];

  return (
    <>
      <div>
        <h1 className="text-lg font-semibold tracking-tight">Viewers</h1>
        <p className="mt-1 text-sm text-muted">
          A viewer opens the published team previews of this tenant. It reaches no sandbox, no
          template and no API key.
        </p>
      </div>

      <Panel
        title="Preview accounts"
        actions={
          can ? (
            <div className="flex items-end gap-2">
              <Field
                label="Email"
                type="email"
                value={email}
                onChange={(event) => setEmail(event.target.value)}
                className="w-52"
              />
              <Field
                label="Password"
                type="password"
                value={password}
                onChange={(event) => setPassword(event.target.value)}
                className="w-40"
              />
              <Button
                variant="primary"
                disabled={!email || !password || add.isPending}
                onClick={() => add.mutate()}
              >
                Add
              </Button>
            </div>
          ) : null
        }
      >
        {add.error ? <div className="p-3"><Notice>{errorText(add.error)}</Notice></div> : null}
        {rows.length === 0 ? (
          <Empty>{viewers.isPending ? "Reading the viewers." : "This tenant has no viewer."}</Empty>
        ) : (
          <Table head={["Email", "Added", ""]}>
            {rows.map((viewer) => (
              <tr key={viewer.id}>
                <td className="px-4 py-2.5">{viewer.email}</td>
                <td className="px-4 py-2.5 text-muted">
                  {viewer.created_at ? new Date(viewer.created_at).toLocaleDateString() : "—"}
                </td>
                <td className="px-4 py-2.5 text-right">
                  {can ? (
                    <Button
                      variant="ghost"
                      onClick={() => {
                        if (window.confirm(`Revoke ${viewer.email}? Open previews stop.`)) {
                          revoke.mutate(viewer.id!);
                        }
                      }}
                    >
                      Revoke
                    </Button>
                  ) : null}
                </td>
              </tr>
            ))}
          </Table>
        )}
      </Panel>
    </>
  );
}
