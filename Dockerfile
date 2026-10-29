# syntax=docker/dockerfile:1

# Hardened Wolfi-based Go images (https://taha2samy-3.github.io/node/), pinned
# by digest and bumped by Dependabot. CI verifies their SLSA provenance before
# building (hack/verify-base-images.sh).
FROM --platform=$BUILDPLATFORM ghcr.io/taha2samy-3/go:1.26-dev@sha256:2ce974cd7962cfdb2fb8f0e2c1e6a4e081a0785c077d8cc141a47a88704c2bcc AS build

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

FROM ghcr.io/taha2samy-3/go:1.26@sha256:e18a4f2c50ca898f014ae3de36ff4e121a03f234203d8e5c9311f8dbe9ac8156

COPY --from=build --chown=0:0 --chmod=0555 /app/webhook /usr/local/bin/webhook

# Numeric UID so Kubernetes can enforce runAsNonRoot.
USER 1000:1000
EXPOSE 8443
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/webhook"]
CMD []
