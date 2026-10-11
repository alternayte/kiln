# @alternayte/kiln

The TypeScript client for [Kiln](https://github.com/alternayte/kiln): isolated
Linux microVMs that restore from a snapshot, fork, sleep, and serve a port on
a public hostname. It uses `fetch`, so it runs on Node.js 18 and later, Bun and
Deno.

```sh
npm install @alternayte/kiln
```

```ts
import { Kiln } from "@alternayte/kiln";

const kiln = new Kiln({ baseUrl: "https://api.example.com", apiKey: "kiln_..." });
await kiln.buildTemplate({
  name: "py312", image: "docker.io/library/python:3.12-slim",
  vcpus: 1, memory_mb: 512, disk_mb: 2048, egress_allow: [],
});
const sandbox = await kiln.createSandboxObject({ template: "py312", lifecycle: "ephemeral", idle_seconds: 600 });
const result = await sandbox.exec(["python3", "-c", "print(2+2)"]);
console.log(result.stdout);
await sandbox.destroy();
```

`exec` streams when you pass `onOutput`. `for await (const event of kiln.events({ after: 0 }))`
reads the event stream. Every method of the generated client is on `Kiln`
too; the [API reference](https://github.com/alternayte/kiln/blob/main/sdk/llms-full.txt)
lists them.

MIT licence. The Kiln host and gateway are AGPL-3.0; this client is not.
