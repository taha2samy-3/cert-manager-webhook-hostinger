# syntax=docker/dockerfile:1

# Hardened Wolfi-based Go images (https://taha2samy-3.github.io/node/), pinned
# by digest and bumped by Dependabot. CI verifies their SLSA provenance before
# building (hack/verify-base-images.sh).
FROM --platform=$BUILDPLATFORM ghcr.io/taha2samy-3/go:v1.27-dev@sha256:10638664bf2618cb9856f8ca7bcbb8358cd61f2be393b53635e18b65eec38442 AS build

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

FROM ghcr.io/taha2samy-3/go:v1.27@sha256:a7967363a353b5aa1e9c248ca76f0b88abd29cff074ab853ac042bc28272f0d4

COPY --from=build --chown=0:0 --chmod=0555 /app/webhook /usr/local/bin/webhook

# Numeric UID so Kubernetes can enforce runAsNonRoot.
USER 1000:1000
EXPOSE 8443
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/webhook"]
CMD []
