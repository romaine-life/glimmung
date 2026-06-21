# syntax=docker/dockerfile:1.7
#
# The live-preview edge image: the standalone reverse-proxy + override receiver
# an app's slot chart runs in front of its backend in a preview test
# environment. Minimal single-binary image — no frontend, no ffmpeg — built from
# the same Go module as the rest of glimmung. Wired into image-build CI via
# .github/workflows/docker-build-check.yaml (matrix entry "live-preview-edge").

FROM golang:1.25-alpine AS backend
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/live-preview-edge ./cmd/live-preview-edge

FROM alpine:3.21 AS runtime
# ca-certificates: the edge validates pushes against auth.romaine.life's HTTPS
# JWKS endpoint, so it needs the public root store to make that TLS call.
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=backend /out/live-preview-edge /app/live-preview-edge
# Default listen port (overridable via LIVE_PREVIEW_EDGE_LISTEN). The Helm
# partial points the pod's served port and probes here.
EXPOSE 8080
USER 1000
ENTRYPOINT ["/app/live-preview-edge"]
