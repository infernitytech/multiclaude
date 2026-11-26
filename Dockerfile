# MultiClaude server image (includes runner binaries for download by machines).
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/multiclaude-server ./cmd/multiclaude-server
RUN set -e; for t in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do \
      GOOS=${t%/*} GOARCH=${t#*/} go build -trimpath -ldflags="-s -w" \
        -o /out/dl/multiclaude-runner-${t%/*}-${t#*/} ./cmd/multiclaude-runner; \
    done

FROM alpine:3.22
RUN apk add --no-cache git git-daemon git-lfs ca-certificates tzdata \
 && git lfs install --system --skip-repo
COPY --from=build /out/multiclaude-server /usr/local/bin/multiclaude-server
COPY --from=build /out/dl /opt/multiclaude/downloads
ENV MC_DATA=/data \
    MC_ADDR=:8090 \
    MC_TLS_ADDR=:8443 \
    MC_DOWNLOADS=/opt/multiclaude/downloads \
    HOME=/data
VOLUME /data
# 8090 = plain HTTP (LAN), 8443 = HTTPS (set MC_DOMAIN or MC_TLS_CERT/KEY)
EXPOSE 8090 8443
HEALTHCHECK --interval=30s --timeout=5s CMD wget -qO- http://127.0.0.1:8090/api/auth >/dev/null || exit 1
# 568 = the "apps" user on TrueNAS SCALE; override with `user:` in compose if needed.
USER 568:568
ENTRYPOINT ["multiclaude-server"]
