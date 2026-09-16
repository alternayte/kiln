import { useEffect, useRef, useState } from "react";
import { Terminal as Xterm } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";

/**
 * SandboxTerminal opens the interactive shell of one sandbox. The socket
 * carries terminal bytes as binary messages both ways, and a text message
 * carries the window size. The session cookie authenticates the upgrade,
 * because a browser cannot set a header on a WebSocket.
 */
export function SandboxTerminal({ id }: { id: string }) {
  const host = useRef<HTMLDivElement>(null);
  const [state, setState] = useState<"opening" | "open" | "closed">("opening");
  const [reason, setReason] = useState("");
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    const element = host.current;
    if (!element) return;

    const term = new Xterm({
      convertEol: false,
      cursorBlink: true,
      fontFamily: '"JetBrains Mono", ui-monospace, monospace',
      fontSize: 13,
      theme: { background: "#17140f", foreground: "#efeae2", cursor: "#f08a3c" },
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(element);
    fit.fit();

    const url = new URL(
      `/v1/sandboxes/${encodeURIComponent(id)}/terminal`,
      window.location.href,
    );
    url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
    url.searchParams.set("cols", String(term.cols));
    url.searchParams.set("rows", String(term.rows));

    const socket = new WebSocket(url);
    socket.binaryType = "arraybuffer";
    const encoder = new TextEncoder();

    socket.onopen = () => setState("open");
    socket.onmessage = (message) => {
      term.write(new Uint8Array(message.data as ArrayBuffer));
    };
    socket.onclose = (event) => {
      setState("closed");
      setReason(event.reason || "the connection closed");
    };
    socket.onerror = () => setReason("the connection failed");

    const typed = term.onData((data) => {
      if (socket.readyState === WebSocket.OPEN) socket.send(encoder.encode(data));
    });

    // The guest needs the size, or every curses program draws at 80x24.
    const sendSize = () => {
      fit.fit();
      if (socket.readyState === WebSocket.OPEN) {
        socket.send(JSON.stringify({ cols: term.cols, rows: term.rows }));
      }
    };
    const observer = new ResizeObserver(sendSize);
    observer.observe(element);

    return () => {
      observer.disconnect();
      typed.dispose();
      socket.close();
      term.dispose();
    };
  }, [id, attempt]);

  return (
    <div className="space-y-2">
      <div className="flex items-center justify-between px-1">
        <span className="text-xs text-muted">
          {state === "open"
            ? "Connected"
            : state === "opening"
              ? "Opening the shell"
              : reason}
        </span>
        {state === "closed" ? (
          <button
            onClick={() => setAttempt((n) => n + 1)}
            className="text-xs text-ember hover:underline"
          >
            Open again
          </button>
        ) : null}
      </div>
      <div
        ref={host}
        className="h-96 overflow-hidden rounded-lg border border-edge bg-[#17140f] p-2"
      />
    </div>
  );
}
