# The gateway image. The host is never built from this file: it needs KVM,
# jailer and its own kernel, and it runs under systemd on bare metal.
# The web UI is built first: the Go build embeds its bundle.
FROM oven/bun:1 AS web
WORKDIR /web
COPY web/package.json web/bun.lock* ./
RUN bun install --frozen-lockfile
COPY sdk/openapi.json /sdk/openapi.json
COPY web/ ./
RUN bun run generate && bun run build

FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /web/dist ./web/dist
# CGO stays off, so the binary needs no libc at run time.
RUN CGO_ENABLED=0 go build -buildvcs=false -trimpath -o /out/kiln-gateway ./cmd/kiln-gateway

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/kiln-gateway /kiln-gateway
# SQLite writes here. A deployment on Postgres needs no volume.
WORKDIR /data
ENV KILN_DB=file:/data/kiln-gateway.db
# The port the gateway serves. A host such as Coolify maps it.
ENV KILN_GATEWAY_ADDR=0.0.0.0:8080
USER nonroot
ENTRYPOINT ["/kiln-gateway"]
