import { useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { deleteSandbox, getSandbox, publish, retire, snapshot } from "@/api";
import { errorText } from "@/lib/auth";
import { useEvents } from "@/lib/events";
import { useCan } from "@/app/session";
import { Button, Empty, Mono, Notice, Panel, State, Table } from "@/components/ui";
import { SandboxTerminal } from "./Terminal";

export function SandboxPage() {
  const { id = "" } = useParams();
  const navigate = useNavigate();
  const queries = useQueryClient();
  const events = useEvents(id);
  const canWrite = useCan("sandbox:write");
  const canExec = useCan("sandbox:exec");

  const sandbox = useQuery({
    queryKey: ["sandbox", id],
    queryFn: async () => (await getSandbox({ path: { id }, throwOnError: true })).data,
  });
  const row = sandbox.data;

  const sleep = useMutation({
    mutationFn: async () =>
      (await snapshot({ path: { id }, body: { stop: true }, throwOnError: true })).data,
    onSuccess: () => queries.invalidateQueries({ queryKey: ["sandbox", id] }),
  });
  const destroy = useMutation({
    mutationFn: async () => (await deleteSandbox({ path: { id }, throwOnError: true })).data,
    onSuccess: () => navigate("/sandboxes", { replace: true }),
  });

  if (sandbox.isPending) return <p className="text-sm text-muted">Reading the sandbox.</p>;
  if (sandbox.isError) return <Notice>{errorText(sandbox.error)}</Notice>;

  return (
    <>
      <div className="flex flex-wrap items-center gap-3">
        <Link to="/sandboxes" className="text-sm text-muted hover:text-ink">
          Sandboxes
        </Link>
        <span className="text-muted">/</span>
        <Mono className="text-sm">{id}</Mono>
        <State value={row?.state} />
        <div className="ml-auto flex gap-2">
          {canWrite ? (
            <>
              <Button onClick={() => sleep.mutate()} disabled={sleep.isPending}>
                Sleep
              </Button>
              <Button
                variant="danger"
                onClick={() => {
                  if (window.confirm("Destroy this sandbox and retire its hostnames?")) {
                    destroy.mutate();
                  }
                }}
              >
                Destroy
              </Button>
            </>
          ) : null}
        </div>
      </div>

      <Notice>{sleep.error ? errorText(sleep.error) : destroy.error ? errorText(destroy.error) : ""}</Notice>

      {canExec ? (
        <Panel title="Terminal" className="p-3">
          <SandboxTerminal id={id} />
        </Panel>
      ) : (
        <Panel title="Terminal">
          <Empty>A reader does not run code in a sandbox.</Empty>
        </Panel>
      )}

      <Ports id={id} ports={row?.published ?? []} canWrite={canWrite} />

      <Panel title="Events">
        {events.length === 0 ? (
          <Empty>Nothing has happened since this page opened.</Empty>
        ) : (
          <Table head={["At", "Transition", "Detail"]}>
            {events.map((event) => (
              <tr key={event.id}>
                <td className="px-4 py-2 text-muted">
                  {event.at ? new Date(event.at).toLocaleTimeString() : "—"}
                </td>
                <td className="px-4 py-2">
                  <Mono>
                    {event.from ?? "—"} → {event.to ?? event.kind ?? "—"}
                  </Mono>
                </td>
                <td className="px-4 py-2 text-muted">{event.detail ?? ""}</td>
              </tr>
            ))}
          </Table>
        )}
      </Panel>
    </>
  );
}

function Ports({
  id,
  ports,
  canWrite,
}: {
  id: string;
  ports: { port?: number; url?: string; visibility?: string }[];
  canWrite: boolean;
}) {
  const queries = useQueryClient();
  const [port, setPort] = useState("");
  const [visibility, setVisibility] = useState<"public" | "team">("public");

  const open = useMutation({
    mutationFn: async () =>
      (
        await publish({
          path: { id },
          body: { port: Number(port), visibility },
          throwOnError: true,
        })
      ).data,
    onSuccess: () => {
      setPort("");
      queries.invalidateQueries({ queryKey: ["sandbox", id] });
    },
  });
  const close = useMutation({
    mutationFn: async (which: number) =>
      (await retire({ path: { id, port: which }, throwOnError: true })).data,
    onSuccess: () => queries.invalidateQueries({ queryKey: ["sandbox", id] }),
  });

  return (
    <Panel
      title="Published ports"
      actions={
        canWrite ? (
          <div className="flex items-center gap-2">
            <input
              value={port}
              onChange={(event) => setPort(event.target.value)}
              placeholder="8000"
              inputMode="numeric"
              aria-label="Guest port"
              className="w-20 rounded-md border border-edge bg-ground px-2 py-1 text-sm"
            />
            <select
              value={visibility}
              onChange={(event) => setVisibility(event.target.value as "public" | "team")}
              aria-label="Visibility"
              className="rounded-md border border-edge bg-ground px-2 py-1 text-sm"
            >
              <option value="public">public</option>
              <option value="team">team</option>
            </select>
            <Button variant="primary" disabled={!port || open.isPending} onClick={() => open.mutate()}>
              Publish
            </Button>
          </div>
        ) : null
      }
    >
      {open.error ? <div className="p-3"><Notice>{errorText(open.error)}</Notice></div> : null}
      {ports.length === 0 ? (
        <Empty>No port is published.</Empty>
      ) : (
        <Table head={["Port", "URL", "Visibility", ""]}>
          {ports.map((entry) => (
            <tr key={entry.port}>
              <td className="px-4 py-2">
                <Mono>{entry.port}</Mono>
              </td>
              <td className="px-4 py-2">
                <a
                  href={entry.url}
                  target="_blank"
                  rel="noreferrer"
                  className="text-ember hover:underline"
                >
                  <Mono>{entry.url}</Mono>
                </a>
              </td>
              <td className="px-4 py-2 text-muted">{entry.visibility}</td>
              <td className="px-4 py-2 text-right">
                {canWrite && entry.port ? (
                  <Button variant="ghost" onClick={() => close.mutate(entry.port!)}>
                    Retire
                  </Button>
                ) : null}
              </td>
            </tr>
          ))}
        </Table>
      )}
    </Panel>
  );
}
