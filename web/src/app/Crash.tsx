import { Component, type ErrorInfo, type ReactNode } from "react";

// A crash must not leave a blank page. The person sees what broke and can
// reload, and the message is the one thing that makes a report useful.
export class Crash extends Component<{ children: ReactNode }, { error: Error | null }> {
  state: { error: Error | null } = { error: null };

  static getDerivedStateFromError(error: Error) {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error("kiln: the UI crashed", error, info.componentStack);
  }

  render() {
    const { error } = this.state;
    if (!error) return this.props.children;
    return (
      <main className="mx-auto max-w-lg space-y-3 p-8">
        <h1 className="text-lg font-semibold">The page stopped</h1>
        <pre className="overflow-x-auto rounded-md border border-danger/40 bg-danger/10 p-3 font-mono text-xs text-danger">
          {error.message}
        </pre>
        <button
          onClick={() => window.location.reload()}
          className="rounded-md bg-ember px-3 py-1.5 text-sm font-medium text-ember-ink"
        >
          Reload
        </button>
      </main>
    );
  }
}
