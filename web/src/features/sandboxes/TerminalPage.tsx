import { useParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";

import { getSandbox } from "@/api";
import { SandboxTerminal } from "./Terminal";

/**
 * TerminalPage is the shell and nothing else, filling the window. It sits
 * outside the app shell, because a person who opens it came to work in the
 * sandbox, not to look at navigation.
 */
export function TerminalPage() {
  const { id = "" } = useParams();
  const sandbox = useQuery({
    queryKey: ["sandbox", id],
    queryFn: async () => (await getSandbox({ path: { id }, throwOnError: true })).data,
  });
  return (
    <div className="h-screen p-3">
      <SandboxTerminal
        id={id}
        template={(sandbox.data as { template?: string } | undefined)?.template}
        full
      />
    </div>
  );
}
