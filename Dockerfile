# syntax=docker/dockerfile:1
# Builds the hub and both agents from the same source; the host only needs Docker.

FROM golang:1.27-bookworm AS build
WORKDIR /src
# nethttpomithttp2: nothing here speaks HTTP/2, and leaving it out saves about 1 MB per binary.
ENV CGO_ENABLED=0 GOFLAGS="-trimpath -tags=nethttpomithttp2"
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    mkdir -p /out/dist /out/data \
 && GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /out/hub ./cmd/hub \
 && GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o /out/dist/btop-agent.exe ./cmd/agent \
 && GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /out/dist/btop-agent-linux-amd64 ./cmd/agent \
 && gzip -9 -n /out/dist/btop-agent.exe /out/dist/btop-agent-linux-amd64

# Nothing but the static hub binary and the agent downloads it serves (gzipped; the hub
# decompresses them on the way out).
FROM scratch
COPY --from=build /out/hub /hub
COPY --from=build /out/dist /dist
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
ENV HUB_LISTEN=:28677 HUB_DATA=/data HUB_DIST=/dist GOMEMLIMIT=24MiB GOMAXPROCS=2
EXPOSE 28677
VOLUME /data
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 CMD ["/hub", "health"]
ENTRYPOINT ["/hub"]
