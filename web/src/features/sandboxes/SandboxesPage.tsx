import { useState } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { createSandbox, listSandboxes, listTemplates } from "@/api";
import { errorText } from "@/lib/auth";
import { useEvents } from "@/lib/events";
import { useCan } from "@/app/session";
import { Button, Empty, Mono, Notice, Panel, State, Table } from "@/components/ui";

export function SandboxesPage() {
  useEvents();
  const can = useCan("sandbox:write");
  const sandboxes = useQuery({
    queryKey: ["sandboxes"],
    queryFn: async () => (await listSandboxes({ throwOnError: true })).data,
  });
  const rows = sandboxes.data ?? [];

  return (
    <>
      <div className="flex items-center justify-between">
        <h1 className="text-lg font-semibold tracking-tight">Sandboxes</h1>
        {can ? <NewSandbox /> : null}
      </div>
      <Panel>
        {rows.length === 0 ? (
          <Empty>
            {sandboxes.isPending
              ? "Reading the sandboxes."
              : "No sandbox runs. Create one from a template."}
          </Empty>
        ) : (
          <Table head={["Id", "Template", "State", "Published", "Created"]}>
            {rows.map((sandbox) => (
              <tr key={sandbox.id} className="hover:bg-ground/40">
                <td className="px-4 py-2.5">
                  <Link to={`/sandboxes/${sandbox.id}`} className="text-ember hover:underline">
                    <Mono>{sandbox.id}</Mono>
                  </Link>
                </td>
                <td className="px-4 py-2.5 text-muted">{sandbox.template}</td>
                <td className="px-4 py-2.5">
                  <State value={sandbox.state} />
                </td>
                <td className="px-4 py-2.5">
                  <Mono className="text-muted">
                    {(sandbox.published ?? []).map((port) => port.url).join(", ") || "—"}
                  </Mono>
                </td>
                <td className="px-4 py-2.5 text-muted">
                  {sandbox.created_at ? new Date(sandbox.created_at).toLocaleString() : "—"}
                </td>
              </tr>
            ))}
          </Table>
        )}
      </Panel>
    </>
  );
}

function NewSandbox() {
  const queries = useQueryClient();
  const [open, setOpen] = useState(false);
  const [template, setTemplate] = useState("");
  const templates = useQuery({
    queryKey: ["templates"],
    queryFn: async () => (await listTemplates({ throwOnError: true })).data,
  });
  const ready = (templates.data ?? []).filter((row) => row.state === "ready");
  const create = useMutation({
    mutationFn: async () =>
      (
        await createSandbox({
          // The host requires a lifecycle and an idle deadline. A sandbox
          // made from this screen is one a person works in, so it survives
          // a sleep and sleeps after half an hour of nobody touching it.
          body: { template, lifecycle: "persistent", idle_seconds: 1800 },
          throwOnError: true,
        })
      ).data,
    onSuccess: () => {
      queries.invalidateQueries({ queryKey: ["sandboxes"] });
      setOpen(false);
    },
  });

  if (!open) {
    return (
      <Button variant="primary" onClick={() => setOpen(true)}>
        New sandbox
      </Button>
    );
  }
  return (
    <div className="flex items-center gap-2">
      <select
        value={template}
        onChange={(event) => setTemplate(event.target.value)}
        className="rounded-md border border-edge bg-raised px-2.5 py-1.5 text-sm"
        aria-label="Template"
      >
        <option value="">Choose a template</option>
        {ready.map((row) => (
          <option key={row.name} value={row.name}>
            {row.name}
          </option>
        ))}
      </select>
      <Button
        variant="primary"
        disabled={!template || create.isPending}
        onClick={() => create.mutate()}
      >
        {create.isPending ? "Creating" : "Create"}
      </Button>
      <Button variant="ghost" onClick={() => setOpen(false)}>
        Cancel
      </Button>
      {create.error ? <Notice>{errorText(create.error)}</Notice> : null}
    </div>
  );
}
