import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { BrowserRouter } from "react-router-dom";

import "./styles/globals.css";
import "./lib/api";
import { App } from "./app/App";
import { Crash } from "./app/Crash";

const queries = new QueryClient({
  defaultOptions: {
    queries: {
      // The event stream pushes changes, so polling would only add load.
      refetchOnWindowFocus: false,
      retry: false,
      staleTime: 5_000,
    },
  },
});

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={queries}>
      <BrowserRouter>
        <Crash>
          <App />
        </Crash>
      </BrowserRouter>
    </QueryClientProvider>
  </StrictMode>,
);
