import { useCallback, useEffect, useRef, useState } from "react";
import { Terminal as Xterm } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import { WebglAddon } from "@xterm/addon-webgl";
import { Copy, Maximize2, Minimize2, RefreshCw } from "lucide-react";
import { Link } from "react-router-dom";

import { cn } from "@/lib/cn";

type Phase = "connecting" | "connected" | "disconnected";

// A dropped socket is usually a wake or a blip, so the shell retries by
// itself and only says so once retrying has stopped helping.
const RETRIES = 4;
const BACKOFF_MS = [500, 1000, 2000, 4000];

/**
 * SandboxTerminal is the interactive shell of one sandbox. The socket carries
 * terminal bytes as binary messages both ways, and a text message carries the
 * window size. The session cookie authenticates the upgrade, because a
 * browser cannot set a header on a WebSocket.
 *
 * It fills its parent and never sets its own height: a shell that is 24 rows
 * tall in a 900px window wastes the window, and a fixed height fights every
 * program that draws a full screen.
 */
export function SandboxTerminal({
  id,
  full,
}: {
  id: string;
  /** True on the standalone page, where the control returns to the tab. */
  full?: boolean;
}) {
  const host = useRef<HTMLDivElement>(null);
  const term = useRef<Xterm | null>(null);
  const fit = useRef<FitAddon | null>(null);
  const [phase, setPhase] = useState<Phase>("connecting");
  const [reason, setReason] = useState("");
  const [attempt, setAttempt] = useState(0);
  const retries = useRef(0);
  const retryTimer = useRef<ReturnType<typeof setTimeout> | null>(null);

  const refit = useCallback(() => {
    try {
      fit.current?.fit();
    } catch {
      // The pane can be measured mid-layout, when it has no size yet. The
      // ResizeObserver calls again once it has one.
    }
  }, []);

  useEffect(() => {
    const element = host.current;
    if (!element) return;

    const xterm = new Xterm({
      cursorBlink: true,
      fontFamily: '"JetBrains Mono", ui-monospace, SFMono-Regular, Menlo, monospace',
      fontSize: 13,
      lineHeight: 1.5,
      // A build log outruns a smaller buffer in seconds.
      scrollback: 10_000,
      theme: {
        // Exactly the app ground, so the shell is one surface with the
        // page and shows no rectangle where it ends.
        background: "#100c0a",
        foreground: "#efeae2",
        cursor: "#f08a3c",
        selectionBackground: "#ffffff30",
      },
    });
    const fitAddon = new FitAddon();
    xterm.loadAddon(fitAddon);
    xterm.open(element);
    // The DOM renderer is visibly slow on a log. WebGL is not everywhere, and
    // a browser can take its context back at any moment, so both cases fall
    // back to the renderer xterm already has.
    let webgl: WebglAddon | null = null;
    try {
      const addon = new WebglAddon();
      addon.onContextLoss(() => {
        addon.dispose();
        webgl = null;
      });
      xterm.loadAddon(addon);
      webgl = addon;
    } catch {
      webgl = null;
    }
    term.current = xterm;
    fit.current = fitAddon;
    refit();

    // The pane is never blank. A black rectangle reads as broken; a line of
    // text reads as a shell that is working.
    xterm.write(`\x1b[2mConnecting to ${id}...\x1b[0m\r\n`);
    setPhase("connecting");
    setReason("");

    const url = new URL(
      `/v1/sandboxes/${encodeURIComponent(id)}/terminal`,
      window.location.href,
    );
    url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
    url.searchParams.set("cols", String(xterm.cols));
    url.searchParams.set("rows", String(xterm.rows));

    const socket = new WebSocket(url);
    socket.binaryType = "arraybuffer";
    const encoder = new TextEncoder();

    socket.onopen = () => {
      retries.current = 0;
      setPhase("connected");
    };
    socket.onmessage = (message) => xterm.write(new Uint8Array(message.data as ArrayBuffer));
    socket.onclose = (event) => {
      const why = event.reason || "the connection closed";
      xterm.write(`\r\n\x1b[2m[${why}]\x1b[0m\r\n`);
      // The shell exiting is the person's own doing, so it never retries.
      const byHand = event.code === 1000;
      if (!byHand && retries.current < RETRIES) {
        const wait = BACKOFF_MS[retries.current] ?? 4000;
        retries.current += 1;
        setPhase("connecting");
        setReason(why);
        xterm.write(`\x1b[2mReconnecting in ${wait / 1000}s...\x1b[0m\r\n`);
        retryTimer.current = setTimeout(() => setAttempt((n) => n + 1), wait);
        return;
      }
      setPhase("disconnected");
      setReason(why);
    };
    socket.onerror = () => setReason("the connection failed");

    const typed = xterm.onData((data) => {
      if (socket.readyState === WebSocket.OPEN) socket.send(encoder.encode(data));
    });

    // The guest needs the size, or every program that draws a screen draws it
    // at 80x24.
    const sendSize = () => {
      refit();
      if (socket.readyState === WebSocket.OPEN) {
        socket.send(JSON.stringify({ cols: xterm.cols, rows: xterm.rows }));
      }
    };
    const observer = new ResizeObserver(sendSize);
    observer.observe(element);

    return () => {
      if (retryTimer.current) clearTimeout(retryTimer.current);
      observer.disconnect();
      typed.dispose();
      socket.close();
      // The addon holds a GPU context that must go before the terminal it
      // draws, or disposal reaches a renderer that is already gone.
      try {
        webgl?.dispose();
      } catch {
        /* already disposed by a context loss */
      }
      xterm.dispose();
      term.current = null;
      fit.current = null;
    };
  }, [id, attempt, refit]);

  // The banner changes the height available to the terminal, so a refit runs
  // when it appears and when it goes.
  useEffect(refit, [phase, refit]);

  return (
    <section className="relative flex h-full min-h-0 flex-col bg-ground">
      {/* The controls float over the shell rather than sitting in a bar above
          it: a bar costs a row of the screen and the shell is the point. */}
      <div className="absolute top-2 right-3 z-10 flex items-center gap-1">
        <Status phase={phase} />
          <IconButton
            label="Copy the output"
            onClick={() => {
              const xterm = term.current;
              if (!xterm) return;
              xterm.selectAll();
              navigator.clipboard.writeText(xterm.getSelection());
              xterm.clearSelection();
            }}
          >
            <Copy className="size-3.5" />
          </IconButton>
          <IconButton
            label="Open the shell again"
            onClick={() => {
              retries.current = 0;
              setAttempt((n) => n + 1);
            }}
          >
            <RefreshCw className="size-3.5" />
          </IconButton>
          <Link
            to={full ? `/sandboxes/${id}/terminal` : `/terminal/${id}`}
            aria-label={full ? "Back to the sandbox" : "Fill the window"}
            title={full ? "Back to the sandbox" : "Fill the window"}
            className="grid size-7 place-items-center rounded text-muted transition-colors hover:bg-edge hover:text-ink"
          >
            {full ? <Minimize2 className="size-3.5" /> : <Maximize2 className="size-3.5" />}
          </Link>
      </div>

      {phase === "disconnected" ? (
        <div className="absolute top-11 right-3 z-10 flex items-center gap-3 rounded-md border border-edge bg-raised px-3 py-2">
          <span className="text-xs text-muted">Disconnected: {reason}</span>
          <button
            onClick={() => {
              retries.current = 0;
              setAttempt((n) => n + 1);
            }}
            className="rounded border border-edge px-2 py-0.5 text-xs text-ink hover:bg-edge"
          >
            Reconnect
          </button>
        </div>
      ) : null}

      <div ref={host} className="min-h-0 flex-1 overflow-hidden px-3 pt-2" />
    </section>
  );
}

// The one word that says whether keystrokes reach a shell.
function Status({ phase }: { phase: Phase }) {
  const tone =
    phase === "connected" ? "text-live" : phase === "connecting" ? "text-warn" : "text-danger";
  return (
    <span className={cn("flex items-center gap-1.5 text-xs", tone)}>
      <span className="size-1.5 rounded-full bg-current" />
      {phase}
    </span>
  );
}

function IconButton({
  label,
  onClick,
  children,
}: {
  label: string;
  onClick: () => void;
  children: React.ReactNode;
}) {
  return (
    <button
      onClick={onClick}
      aria-label={label}
      title={label}
      className="grid size-7 place-items-center rounded text-muted transition-colors hover:bg-edge hover:text-ink"
    >
      {children}
    </button>
  );
}
