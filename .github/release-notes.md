Install a host. It needs KVM, and no Go toolchain and no source.

```sh
curl -fsSLO https://github.com/alternayte/kiln/releases/latest/download/kilnd
curl -fsSLO https://github.com/alternayte/kiln/releases/latest/download/SHA256SUMS
sha256sum --ignore-missing -c SHA256SUMS
sudo install -m755 kilnd /usr/local/bin/kilnd
sudo kilnd init --zone example.com --acme-email you@example.com
sudo kilnd serve
```

The binary carries the guest agent, so `kilnd init` writes it out. The
[README](https://github.com/alternayte/kiln#readme) carries the first
sandbox; [docs/gateway.md](https://github.com/alternayte/kiln/blob/main/docs/gateway.md)
carries the gateway.

`kiln-<os>-<arch>` is the client, for Linux and macOS. An upgrade from a release
before v0.10.0 replaces the `kiln` binary and its unit:
[infra/README.md](https://github.com/alternayte/kiln/blob/main/infra/README.md#upgrade-from-the-kiln-binary-to-kilnd).

The gateway image is `ghcr.io/alternayte/kiln-gateway` at this tag.

See `LOG.md` for what this release changed and what running it taught.
