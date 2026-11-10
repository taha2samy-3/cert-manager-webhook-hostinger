# syntax=docker/dockerfile:1

# Hardened Wolfi-based Go images (https://taha2samy-3.github.io/node/), pinned
# by digest and bumped by Dependabot. CI verifies their SLSA provenance before
# building (hack/verify-base-images.sh).
FROM --platform=$BUILDPLATFORM ghcr.io/taha2samy-3/go:v1.27-dev@sha256:21a6f84a5f7de3a9c9acfd445a5ffe520b6ebb416757ab3798e1792aaca267d6 AS build

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

FROM ghcr.io/taha2samy-3/go:v1.27@sha256:52f1c0320f1a6f95f8306c6da3869a83a37cf2185e6bba4ea97dc4ba94b23009

COPY --from=build --chown=0:0 --chmod=0555 /app/webhook /usr/local/bin/webhook

# Numeric UID so Kubernetes can enforce runAsNonRoot.
USER 1000:1000
EXPOSE 8443
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/webhook"]
CMD []
