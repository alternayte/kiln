import { useEffect, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";

/** One row of the tenant event stream. */
export interface KilnEvent {
  id: number;
  sandbox_id?: string;
  kind?: string;
  from?: string;
  to?: string;
  at?: string;
  detail?: string;
}

/**
 * useEvents reads the tenant event stream. Every event invalidates the lists,
 * so a build that finishes or a sandbox that sleeps appears with no refresh.
 * The session cookie authenticates it, so EventSource needs no credential.
 */
export function useEvents(sandboxId?: string): KilnEvent[] {
  const queries = useQueryClient();
  const [events, setEvents] = useState<KilnEvent[]>([]);

  useEffect(() => {
    const url = sandboxId
      ? `/v1/events?sandbox_id=${encodeURIComponent(sandboxId)}`
      : "/v1/events";
    const source = new EventSource(url, { withCredentials: true });
    source.addEventListener("transition", (message) => {
      let event: KilnEvent;
      try {
        event = JSON.parse((message as MessageEvent).data);
      } catch {
        return;
      }
      // The newest event is first, and the list is bounded: a long-open tab
      // must not grow without end.
      setEvents((current) => [event, ...current].slice(0, 200));
      queries.invalidateQueries({ queryKey: ["sandboxes"] });
      queries.invalidateQueries({ queryKey: ["templates"] });
      if (event.sandbox_id) {
        queries.invalidateQueries({ queryKey: ["sandbox", event.sandbox_id] });
      }
    });
    return () => source.close();
  }, [sandboxId, queries]);

  return events;
}
