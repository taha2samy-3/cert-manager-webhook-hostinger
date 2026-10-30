# syntax=docker/dockerfile:1

# Hardened Wolfi-based Go images (https://taha2samy-3.github.io/node/), pinned
# by digest and bumped by Dependabot. CI verifies their SLSA provenance before
# building (hack/verify-base-images.sh).
FROM --platform=$BUILDPLATFORM ghcr.io/taha2samy-3/go:v1.27-dev@sha256:eea1fc8b8565961801e9b2ed0f5e419dcbac8c298ab119ab6e7ea7208bbc7a02 AS build

WORKDIR /app/src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/app/go/pkg/mod,uid=1000,gid=1000 \
    go mod download

COPY cmd/ cmd/
COPY internal/ internal/

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN --mount=type=cache,target=/app/go/pkg/mod,uid=1000,gid=1000 \
    --mount=type=cache,target=/app/.cache/go-build,uid=1000,gid=1000 \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH GOCACHE=/app/.cache/go-build \
    go build -trimpath -buildvcs=false \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /app/webhook ./cmd/webhook

FROM ghcr.io/taha2samy-3/go:v1.27@sha256:e11f13b954b1c02cb32b130a729f859fbd80c2b72650413f032bd45d63e511e9

COPY --from=build --chown=0:0 --chmod=0555 /app/webhook /usr/local/bin/webhook

# Numeric UID so Kubernetes can enforce runAsNonRoot.
USER 1000:1000
EXPOSE 8443
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/webhook"]
CMD []
