# syntax=docker/dockerfile:1

# Hardened Wolfi-based Go images (https://taha2samy-3.github.io/node/), pinned
# by digest and bumped by Dependabot. CI verifies their SLSA provenance before
# building (hack/verify-base-images.sh).
FROM --platform=$BUILDPLATFORM ghcr.io/taha2samy-3/go:v1.27-dev@sha256:afd9c81e99ebe05b6baa3ceac9c9089fa75c8cdb44b874e8173391997e265dfc AS build

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

FROM ghcr.io/taha2samy-3/go:v1.27@sha256:a708c7078b9b129a251415e1b4ed1c6b699a85a019277c485f4b9a0af67c8280

COPY --from=build --chown=0:0 --chmod=0555 /app/webhook /usr/local/bin/webhook

# Numeric UID so Kubernetes can enforce runAsNonRoot.
USER 1000:1000
EXPOSE 8443
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/webhook"]
CMD []
