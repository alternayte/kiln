import { useParams } from "react-router-dom";

import { SandboxTerminal } from "./Terminal";

/**
 * TerminalPage is the shell and nothing else, filling the window. It sits
 * outside the app shell, because a person who opens it came to work in the
 * sandbox, not to look at navigation.
 */
export function TerminalPage() {
  const { id = "" } = useParams();
  return (
    <div className="h-screen">
      <SandboxTerminal id={id} full />
    </div>
  );
}
