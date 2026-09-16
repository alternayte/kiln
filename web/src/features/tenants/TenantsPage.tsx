import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { errorText } from "@/lib/auth";
import { useIsOperator } from "@/app/session";
import { Button, Empty, Field, Mono, Notice, Panel, Table } from "@/components/ui";

// The operator routes live on the gateway, not on /v1, so they are called
// directly. They are the only screen an editor never sees.
interface Tenant {
  id: string;
  name?: string;
  max_sandboxes?: number;
  max_templates?: number;
  max_snapshot_bytes?: number;
  sandboxes?: number;
  templates?: number;
  snapshot_bytes?: number;
}

async function operator<T>(method: string, path: string, body?: unknown): Promise<T> {
  const response = await fetch(path, {
    method,
    credentials: "include",
    headers: body === undefined ? {} : { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await response.text();
  const payload = text ? JSON.parse(text) : undefined;
  if (!response.ok) throw payload ?? new Error(response.statusText);
  return payload as T;
}

export function TenantsPage() {
  const isOperator = useIsOperator();
  const queries = useQueryClient();
  const [name, setName] = useState("");
  const [slug, setSlug] = useState("");

  const tenants = useQuery({
    queryKey: ["tenants"],
    enabled: isOperator,
    queryFn: () => operator<{ tenants?: Tenant[] }>("GET", "/operator/tenants"),
  });

  const create = useMutation({
    mutationFn: () => operator<Tenant>("POST", "/operator/tenants", { name, slug }),
    onSuccess: () => {
      setName("");
      setSlug("");
      queries.invalidateQueries({ queryKey: ["tenants"] });
    },
  });

  if (!isOperator) {
    return <Empty>Only an operator administers tenants.</Empty>;
  }

  const rows = tenants.data?.tenants ?? [];

  return (
    <>
      <h1 className="text-lg font-semibold tracking-tight">Tenants</h1>

      <Panel
        title="Every tenant on this host"
        actions={
          <div className="flex items-end gap-2">
            <Field
              label="Name"
              value={name}
              onChange={(event) => setName(event.target.value)}
              className="w-40"
            />
            <Field
              label="Slug"
              value={slug}
              onChange={(event) => setSlug(event.target.value)}
              placeholder="acme"
              className="w-32"
            />
            <Button
              variant="primary"
              disabled={!name || !slug || create.isPending}
              onClick={() => create.mutate()}
            >
              Create
            </Button>
          </div>
        }
      >
        {create.error ? <div className="p-3"><Notice>{errorText(create.error)}</Notice></div> : null}
        {rows.length === 0 ? (
          <Empty>{tenants.isPending ? "Reading the tenants." : "No tenant exists."}</Empty>
        ) : (
          <Table head={["Tenant", "Sandboxes", "Templates", "Snapshot bytes", ""]}>
            {rows.map((tenant) => (
              <TenantRow key={tenant.id} tenant={tenant} />
            ))}
          </Table>
        )}
      </Panel>
    </>
  );
}

function TenantRow({ tenant }: { tenant: Tenant }) {
  const queries = useQueryClient();
  const [caps, setCaps] = useState({
    max_sandboxes: tenant.max_sandboxes ?? 0,
    max_templates: tenant.max_templates ?? 0,
    max_snapshot_bytes: tenant.max_snapshot_bytes ?? 0,
  });
  const [open, setOpen] = useState(false);

  const save = useMutation({
    mutationFn: () =>
      operator<Tenant>("PUT", `/operator/tenants/${encodeURIComponent(tenant.id)}/caps`, caps),
    onSuccess: () => {
      setOpen(false);
      queries.invalidateQueries({ queryKey: ["tenants"] });
    },
  });

  // A zero cap is no limit, so it reads as a dash rather than as zero.
  const cap = (value?: number) => (value ? value.toLocaleString() : "no limit");

  return (
    <tr>
      <td className="px-4 py-2.5">
        <div>{tenant.name ?? "—"}</div>
        <Mono className="text-muted">{tenant.id}</Mono>
      </td>
      <td className="px-4 py-2.5">
        {tenant.sandboxes ?? 0} / {cap(tenant.max_sandboxes)}
      </td>
      <td className="px-4 py-2.5">
        {tenant.templates ?? 0} / {cap(tenant.max_templates)}
      </td>
      <td className="px-4 py-2.5">
        {(tenant.snapshot_bytes ?? 0).toLocaleString()} / {cap(tenant.max_snapshot_bytes)}
      </td>
      <td className="px-4 py-2.5 text-right">
        {open ? (
          <div className="flex items-center justify-end gap-2">
            {(["max_sandboxes", "max_templates", "max_snapshot_bytes"] as const).map((key) => (
              <input
                key={key}
                value={caps[key]}
                inputMode="numeric"
                aria-label={key}
                onChange={(event) =>
                  setCaps({ ...caps, [key]: Number(event.target.value) || 0 })
                }
                className="w-24 rounded-md border border-edge bg-ground px-2 py-1 text-sm"
              />
            ))}
            <Button variant="primary" disabled={save.isPending} onClick={() => save.mutate()}>
              Save
            </Button>
            <Button variant="ghost" onClick={() => setOpen(false)}>
              Cancel
            </Button>
          </div>
        ) : (
          <Button variant="ghost" onClick={() => setOpen(true)}>
            Set caps
          </Button>
        )}
      </td>
    </tr>
  );
}
