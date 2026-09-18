# syntax=docker/dockerfile:1

# ---------- build ----------
FROM golang:1.26-alpine AS build

# The race detector requires cgo, and the alpine Go image ships no C toolchain.
# Without these the test step below fails with "gcc: executable file not found".
RUN apk add --no-cache gcc musl-dev

WORKDIR /src

# Dependencies are resolved first so this layer is reused whenever only
# application code changes, which is most of the time.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Tests run inside the build so a broken commit cannot produce a publishable
# image. Set RUN_TESTS=0 to skip them when iterating on the image itself.
ARG RUN_TESTS=1
RUN if [ "$RUN_TESTS" = "1" ]; then CGO_ENABLED=1 go test -race ./...; else echo "skipping tests"; fi

# CGO_ENABLED=0 produces a fully static binary, which is what allows the runtime
# stage to be distroless static with no libc at all. -trimpath removes local
# filesystem paths; the ldflags strip the symbol table and DWARF data.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/server ./cmd/server

# ---------- runtime ----------
# distroless static: no shell, no package manager, no utilities. The attack
# surface is the binary and nothing else.
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

COPY --from=build /out/server /app/server
COPY --from=build /src/deploy/config.yaml /app/config.yaml

# uid 65532. Nothing here writes to disk or binds a privileged port.
USER nonroot:nonroot

EXPOSE 8080

# The image has no curl or wget, so the binary probes itself. See probeHealth in
# cmd/server. Compose gates the load balancer on this passing.
HEALTHCHECK --interval=10s --timeout=5s --start-period=5s --retries=5 \
    CMD ["/app/server", "-healthcheck", "-config", "/app/config.yaml"]

ENTRYPOINT ["/app/server"]
CMD ["-config", "/app/config.yaml"]
