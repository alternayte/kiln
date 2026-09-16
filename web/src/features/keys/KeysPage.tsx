import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { authClient, errorText } from "@/lib/auth";
import { useMe } from "@/app/session";
import { Button, Empty, Field, Mono, Notice, Panel, Table } from "@/components/ui";

export function KeysPage() {
  const queries = useQueryClient();
  const me = useMe();
  const [name, setName] = useState("");
  // The plaintext exists only in this answer. The gateway stores a hash, so
  // a second view is impossible.
  const [plaintext, setPlaintext] = useState("");

  const keys = useQuery({
    queryKey: ["api-keys"],
    queryFn: () => authClient.apiKeys.listKeys(),
  });

  const create = useMutation({
    mutationFn: () =>
      authClient.apiKeys.createKey({ name, orgId: me.data?.tenant_id ?? undefined }),
    onSuccess: (answer) => {
      setPlaintext(answer.plaintext);
      setName("");
      queries.invalidateQueries({ queryKey: ["api-keys"] });
    },
  });

  const revoke = useMutation({
    mutationFn: (id: string) => authClient.apiKeys.revokeKey(id),
    onSuccess: () => queries.invalidateQueries({ queryKey: ["api-keys"] }),
  });

  const rows = (keys.data?.keys ?? []).filter((key) => !key.revokedAt);

  return (
    <>
      <h1 className="text-lg font-semibold tracking-tight">API keys</h1>

      {plaintext ? (
        <Panel title="Copy this key now">
          <div className="space-y-3 p-4">
            <p className="text-sm text-muted">
              This is the only time the key is shown. Kiln stores a hash of it.
            </p>
            <div className="flex items-center gap-2">
              <code className="flex-1 overflow-x-auto rounded-md border border-ember/40 bg-ground px-3 py-2 font-mono text-sm text-ember">
                {plaintext}
              </code>
              <Button onClick={() => navigator.clipboard.writeText(plaintext)}>Copy</Button>
              <Button variant="ghost" onClick={() => setPlaintext("")}>
                Done
              </Button>
            </div>
          </div>
        </Panel>
      ) : null}

      <Panel
        title="Keys of this tenant"
        actions={
          <div className="flex items-end gap-2">
            <Field
              label="Name"
              value={name}
              onChange={(event) => setName(event.target.value)}
              placeholder="ci"
              className="w-40"
            />
            <Button variant="primary" disabled={!name || create.isPending} onClick={() => create.mutate()}>
              Create
            </Button>
          </div>
        }
      >
        {create.error ? <div className="p-3"><Notice>{errorText(create.error)}</Notice></div> : null}
        {rows.length === 0 ? (
          <Empty>{keys.isPending ? "Reading the keys." : "This tenant holds no key."}</Empty>
        ) : (
          <Table head={["Name", "Starts with", "Created", "Last used", ""]}>
            {rows.map((key) => (
              <tr key={key.id}>
                <td className="px-4 py-2.5">{key.name}</td>
                <td className="px-4 py-2.5">
                  <Mono className="text-muted">{key.start}…</Mono>
                </td>
                <td className="px-4 py-2.5 text-muted">
                  {new Date(key.createdAt).toLocaleDateString()}
                </td>
                <td className="px-4 py-2.5 text-muted">
                  {key.lastUsedAt ? new Date(key.lastUsedAt).toLocaleString() : "never"}
                </td>
                <td className="px-4 py-2.5 text-right">
                  <Button
                    variant="ghost"
                    onClick={() => {
                      if (window.confirm(`Revoke the key ${key.name}? Callers using it stop.`)) {
                        revoke.mutate(key.id);
                      }
                    }}
                  >
                    Revoke
                  </Button>
                </td>
              </tr>
            ))}
          </Table>
        )}
      </Panel>
    </>
  );
}
