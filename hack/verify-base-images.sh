#!/usr/bin/env bash
# Verifies the SLSA build provenance of every base image referenced by the
# given Dockerfiles: each must be pinned by digest and attested by the
# hardened-images builder workflow. Requires gh and GH_TOKEN.
set -euo pipefail
OWNER="taha2samy-3"
SIGNER="taha2samy-3/node/.github/workflows/build-images.yml"

status=0
while read -r image; do
  if [[ "$image" != *@sha256:* ]]; then
    echo "::error::base image $image is not pinned by digest"
    status=1
    continue
  fi
  echo "Verifying $image"
  if ! gh attestation verify "oci://$image" --owner "$OWNER" --signer-workflow "$SIGNER" >/dev/null; then
    echo "::error::provenance verification failed for $image"
    status=1
  fi
done < <(sed -nE 's/^FROM( --platform=[^ ]+)? ([^ ]+).*/\2/p' "${@:-Dockerfile}" | sort -u)
exit $status
