Install a host. It needs KVM, and no Go toolchain and no source.

```sh
curl -fsSLO https://github.com/alternayte/kiln/releases/latest/download/kiln
curl -fsSLO https://github.com/alternayte/kiln/releases/latest/download/SHA256SUMS
sha256sum -c SHA256SUMS
sudo install -m755 kiln /usr/local/bin/kiln
sudo kiln init --zone example.com --acme-email you@example.com
sudo kiln serve
```

The binary carries the guest agent, so `kiln init` writes it out.

The gateway image is `ghcr.io/alternayte/kiln-gateway` at this tag.

See `LOG.md` for what this release changed and what running it taught.
