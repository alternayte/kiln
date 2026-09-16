import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { createTemplate, deleteTemplate, listTemplates } from "@/api";
import { errorText } from "@/lib/auth";
import { useEvents } from "@/lib/events";
import { useCan } from "@/app/session";
import { Button, Empty, Field, Mono, Notice, Panel, State, Table } from "@/components/ui";

export function TemplatesPage() {
  useEvents();
  const queries = useQueryClient();
  const can = useCan("template:write");
  const [open, setOpen] = useState(false);

  const templates = useQuery({
    queryKey: ["templates"],
    queryFn: async () => (await listTemplates({ throwOnError: true })).data,
  });
  const rows = templates.data ?? [];

  const remove = useMutation({
    mutationFn: async (name: string) =>
      (await deleteTemplate({ path: { name }, throwOnError: true })).data,
    onSuccess: () => queries.invalidateQueries({ queryKey: ["templates"] }),
  });

  return (
    <>
      <div className="flex items-center justify-between">
        <h1 className="text-lg font-semibold tracking-tight">Templates</h1>
        {can ? (
          <Button variant="primary" onClick={() => setOpen((v) => !v)}>
            {open ? "Cancel" : "Build a template"}
          </Button>
        ) : null}
      </div>

      {open ? <BuildForm onDone={() => setOpen(false)} /> : null}

      <Panel>
        {remove.error ? <div className="p-3"><Notice>{errorText(remove.error)}</Notice></div> : null}
        {rows.length === 0 ? (
          <Empty>
            {templates.isPending ? "Reading the templates." : "No template is built yet."}
          </Empty>
        ) : (
          <Table head={["Name", "Image", "State", "Size", ""]}>
            {rows.map((template) => (
              <tr key={template.name}>
                <td className="px-4 py-2.5">
                  <Mono>{template.name}</Mono>
                </td>
                <td className="px-4 py-2.5 text-muted">{template.image}</td>
                <td className="px-4 py-2.5">
                  <State value={template.state} />
                  {template.error ? (
                    <p className="mt-1 text-xs text-danger">{template.error}</p>
                  ) : null}
                </td>
                <td className="px-4 py-2.5 text-muted">
                  {template.vcpus ?? "—"} vCPU · {template.memory_mb ?? "—"} MB
                </td>
                <td className="px-4 py-2.5 text-right">
                  {can ? (
                    <Button
                      variant="ghost"
                      onClick={() => {
                        if (window.confirm(`Delete the template ${template.name}?`)) {
                          remove.mutate(template.name!);
                        }
                      }}
                    >
                      Delete
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

function BuildForm({ onDone }: { onDone: () => void }) {
  const queries = useQueryClient();
  const [name, setName] = useState("");
  const [image, setImage] = useState("");
  const [egress, setEgress] = useState("");
  const [setup, setSetup] = useState("");

  const build = useMutation({
    mutationFn: async () =>
      (
        await createTemplate({
          body: {
            name,
            image,
            // Egress is deny by default, so an empty list reaches nothing.
            egress_allow: egress
              .split(/[\s,]+/)
              .map((host) => host.trim())
              .filter(Boolean),
            setup: setup
              .split("\n")
              .map((line) => line.trim())
              .filter(Boolean),
          },
          throwOnError: true,
        })
      ).data,
    onSuccess: () => {
      queries.invalidateQueries({ queryKey: ["templates"] });
      onDone();
    },
  });

  return (
    <Panel title="Build a template">
      <div className="space-y-4 p-4">
        <div className="grid gap-4 sm:grid-cols-2">
          <Field
            label="Name"
            value={name}
            onChange={(event) => setName(event.target.value)}
            placeholder="python"
          />
          <Field
            label="Image"
            value={image}
            onChange={(event) => setImage(event.target.value)}
            placeholder="docker.io/library/python:3.12-slim"
          />
        </div>
        <Field
          label="Egress allow"
          value={egress}
          onChange={(event) => setEgress(event.target.value)}
          placeholder="files.pythonhosted.org"
          hint="Hostnames a sandbox may reach. Empty allows no egress."
        />
        <label className="block space-y-1.5">
          <span className="text-xs font-medium tracking-wide text-muted uppercase">
            Setup commands
          </span>
          <textarea
            value={setup}
            onChange={(event) => setSetup(event.target.value)}
            rows={3}
            placeholder="pip install numpy"
            className="w-full rounded-md border border-edge bg-ground px-3 py-2 font-mono text-sm"
          />
          <span className="block text-xs text-muted">One command per line. They run once.</span>
        </label>
        {build.error ? <Notice>{errorText(build.error)}</Notice> : null}
        <Button
          variant="primary"
          disabled={!name || !image || build.isPending}
          onClick={() => build.mutate()}
        >
          {build.isPending ? "Starting the build" : "Build"}
        </Button>
      </div>
    </Panel>
  );
}
