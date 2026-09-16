import type { ReactNode, ButtonHTMLAttributes, InputHTMLAttributes } from "react";
import { cn } from "@/lib/cn";

// The primitives the screens share. Kiln's surface is small, so the set is
// small: a button, a field, a panel, a table and a state dot.

export function Button({
  variant = "default",
  className,
  ...rest
}: ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: "default" | "primary" | "danger" | "ghost";
}) {
  return (
    <button
      {...rest}
      className={cn(
        "inline-flex items-center justify-center gap-2 rounded-md px-3 py-1.5 text-sm font-medium",
        "transition-colors disabled:cursor-not-allowed disabled:opacity-40",
        variant === "default" && "border border-edge bg-raised text-ink hover:bg-edge",
        variant === "primary" && "bg-ember text-ember-ink hover:brightness-110",
        variant === "danger" && "border border-edge text-danger hover:bg-danger/10",
        variant === "ghost" && "text-muted hover:text-ink",
        className,
      )}
    />
  );
}

export function Field({
  label,
  hint,
  className,
  ...rest
}: InputHTMLAttributes<HTMLInputElement> & { label: string; hint?: string }) {
  return (
    <label className="block space-y-1.5">
      <span className="block text-xs font-medium tracking-wide text-muted uppercase">{label}</span>
      <input
        {...rest}
        className={cn(
          "w-full rounded-md border border-edge bg-ground px-3 py-2 text-sm",
          "placeholder:text-muted/60 focus:border-ember focus:outline-none",
          className,
        )}
      />
      {hint ? <span className="block text-xs text-muted">{hint}</span> : null}
    </label>
  );
}

export function Panel({
  title,
  actions,
  children,
  className,
}: {
  title?: ReactNode;
  actions?: ReactNode;
  children: ReactNode;
  className?: string;
}) {
  return (
    <section className={cn("overflow-hidden rounded-lg border border-edge bg-raised", className)}>
      {title || actions ? (
        <header className="flex flex-wrap items-center justify-between gap-3 border-b border-edge px-4 py-3">
          <h2 className="text-sm font-semibold">{title}</h2>
          <div className="flex items-center gap-2">{actions}</div>
        </header>
      ) : null}
      {children}
    </section>
  );
}

/** A sandbox or template state, as a word and a colour. */
export function State({ value }: { value?: string }) {
  const tone =
    value === "running" || value === "ready"
      ? "text-live"
      : value === "failed" || value === "error"
        ? "text-danger"
        : value === "building" || value === "starting" || value === "pending"
          ? "text-warn"
          : "text-muted";
  return (
    <span className={cn("inline-flex items-center gap-1.5 text-xs font-medium", tone)}>
      <span className="size-1.5 rounded-full bg-current" />
      {value ?? "unknown"}
    </span>
  );
}

export function Mono({ children, className }: { children: ReactNode; className?: string }) {
  return <span className={cn("font-mono text-xs", className)}>{children}</span>;
}

export function Empty({ children }: { children: ReactNode }) {
  return <p className="px-4 py-10 text-center text-sm text-muted">{children}</p>;
}

export function Notice({ children }: { children: ReactNode }) {
  if (!children) return null;
  return (
    <p className="rounded-md border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">
      {children}
    </p>
  );
}

export function Table({
  head,
  children,
}: {
  head: ReactNode[];
  children: ReactNode;
}) {
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-sm">
        <thead>
          <tr className="border-b border-edge text-left">
            {head.map((cell, index) => (
              <th
                key={index}
                className="px-4 py-2 text-xs font-medium tracking-wide text-muted uppercase"
              >
                {cell}
              </th>
            ))}
          </tr>
        </thead>
        <tbody className="divide-y divide-edge">{children}</tbody>
      </table>
    </div>
  );
}
