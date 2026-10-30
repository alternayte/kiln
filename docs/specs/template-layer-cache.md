# Template layer cache

Closes the rest of alternayte/kiln#15.

## What it does
The host keeps the image layers each tenant has pulled. A new build of the same tenant pulls only the layers it does not hold, so a second commit on a pull request builds faster. The `layers` event says how many layers came from the cache. A build also stops wasting the shutdown grace on each of its two boots.

## Decisions
- One cache per tenant, at `layers/<tenant>/` under the Kiln root — a tenant reuses only what it pulled with its own credential, so no permission check against the registry is needed for a layer.
- A layer's name is its digest, and the host checks the digest before it keeps a file — the name then proves the content, and a corrupt download never enters the cache.
- A build still reads the manifest from the registry with its credential — a tenant that lost access to an image fails even when it holds every layer.
- The cache holds 4 GiB per tenant and removes the least recently used layers first, after each pull — the Dedibox has a 20 GB disk with 8.5 GB free.
- A build always applies a layer from the file on disk — one path for a hit and a miss.
- Deleting a tenant deletes its cache — the layers are that tenant's data.
- `kilninit` restarts the guest instead of powering it off — Firecracker exits on the keyboard controller reset that `reboot=k` sends, and a power off left the host to wait out the 5 second grace and kill the VM.

## Out
- A cache shared between tenants.
- An extracted tree cache. The phase numbers decide this after the layer cache lands.
- A cap per host, or a setting for the cap.

## How I know it works
- A second commit on a c-dash pull request, with no change to its packages, prints a `layers` phase well below the first, and says how many layers were cached.
- `phase setup` of a template with no setup commands is well under 5 seconds.
- A build with a wrong `credential` fails on a host that holds every layer of the image.
- A corrupt file written to the cache by hand is pulled again.
