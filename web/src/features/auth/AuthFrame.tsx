import type { ReactNode } from "react";

/** The frame the three unauthenticated screens share. */
export function AuthFrame({
  title,
  subtitle,
  children,
}: {
  title: string;
  subtitle?: string;
  children?: ReactNode;
}) {
  return (
    <div className="grid min-h-screen place-items-center px-4">
      <main className="w-full max-w-sm space-y-6">
        <div className="space-y-1">
          <p className="font-mono text-sm font-semibold">
            <span className="text-ember">▲</span> kiln
          </p>
          <h1 className="text-xl font-semibold tracking-tight">{title}</h1>
          {subtitle ? <p className="text-sm text-muted">{subtitle}</p> : null}
        </div>
        {children}
      </main>
    </div>
  );
}
