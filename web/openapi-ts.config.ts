import { defineConfig } from "@hey-api/openapi-ts";

// The document comes from internal/apispec, so the UI restates no route and
// a route change breaks this build.
export default defineConfig({
  input: "../sdk/openapi.json",
  output: "src/api",
  plugins: ["@hey-api/client-fetch"],
});
