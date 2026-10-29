<p align="center">
  <img src="https://raw.githubusercontent.com/taha2samy-3/cert-manager-webhook-hostinger/main/assets/icon.svg" width="128" alt="">
</p>

# cert-manager-webhook-hostinger

[![CI](https://github.com/taha2samy-3/cert-manager-webhook-hostinger/actions/workflows/ci.yml/badge.svg)](https://github.com/taha2samy-3/cert-manager-webhook-hostinger/actions/workflows/ci.yml)
[![SLSA 3](https://slsa.dev/images/gh-badge-level3.svg)](https://slsa.dev)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/taha2samy-3/cert-manager-webhook-hostinger/badge)](https://scorecard.dev/viewer/?uri=github.com/taha2samy-3/cert-manager-webhook-hostinger)
[![Artifact Hub](https://img.shields.io/endpoint?url=https://artifacthub.io/badge/repository/cert-manager-webhook-hostinger)](https://artifacthub.io/packages/search?repo=cert-manager-webhook-hostinger)

A [cert-manager](https://cert-manager.io) ACME **DNS01** solver for domains hosted on
[Hostinger DNS](https://www.hostinger.com). It lets cert-manager issue and renew
Let's Encrypt (or any ACME) certificates, including **wildcards**, by creating the
`_acme-challenge` TXT records through the
[Hostinger API](https://developers.hostinger.com).

- Works with `Certificate`, `Ingress` and **Gateway API** resources.
- Renewal is automatic: cert-manager renews before expiry and calls the webhook again.
- Apex + wildcard (`example.com` and `*.example.com`) in one certificate. Both challenges
  share the same TXT name, and the webhook keeps both values.
- Passes cert-manager's DNS01 conformance suite in strict mode.
- Hardened, non-root, read-only image built on
  [Wolfi-based hardened Go images](https://taha2samy-3.github.io/node/).
- Images and charts are signed and ship SLSA Build Level 3 provenance and an SBOM.

> This is a community project, not affiliated with or endorsed by Hostinger or the
> cert-manager project. Logos belong to their respective owners.

## Requirements

- Kubernetes 1.27+
- cert-manager 1.15+ ([install](https://cert-manager.io/docs/installation/helm/))
- A Hostinger API token: hPanel → Profile → **API**

## Install

```sh
helm install hostinger-webhook \
  oci://ghcr.io/taha2samy-3/charts/cert-manager-webhook-hostinger \
  --namespace cert-manager
```

Install it in the namespace where cert-manager runs. If cert-manager uses a
different namespace or ServiceAccount, set `certManager.namespace` and
`certManager.serviceAccountName`.

## Configure

### 1. Store the API token

```sh
kubectl -n cert-manager create secret generic hostinger-credentials \
  --from-literal=api-token='<HOSTINGER_API_TOKEN>'
```

A `ClusterIssuer` reads the Secret from cert-manager's *cluster resource namespace*
(`cert-manager` by default). An `Issuer` reads it from its own namespace. Add those
namespaces to `secretAccess.namespaces` so the webhook is allowed to read them.

### 2. Create an issuer

```yaml
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: letsencrypt
spec:
  acme:
    server: https://acme-v02.api.letsencrypt.org/directory
    email: you@example.com
    privateKeySecretRef:
      name: letsencrypt-account
    solvers:
      - dns01:
          webhook:
            groupName: acme.hostinger.taha2samy-3.github.io
            solverName: hostinger
            config:
              apiTokenSecretRef:
                name: hostinger-credentials
                key: api-token
```

Try `https://acme-staging-v02.api.letsencrypt.org/directory` first to avoid Let's
Encrypt rate limits.

### 3. Request certificates

**Certificate**

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: example-com
spec:
  secretName: example-com-tls
  issuerRef:
    kind: ClusterIssuer
    name: letsencrypt
  dnsNames:
    - example.com
    - "*.example.com"
```

**Gateway API.** Enable cert-manager's Gateway support
(`--set config.enableGatewayAPI=true` on the cert-manager chart, together with
`config.apiVersion=controller.config.cert-manager.io/v1alpha1` and
`config.kind=ControllerConfiguration`). Then annotate the Gateway:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: web
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt
spec:
  gatewayClassName: <your-class>
  listeners:
    - name: https
      protocol: HTTPS
      port: 443
      hostname: "*.example.com"
      tls:
        mode: Terminate
        certificateRefs:
          - name: wildcard-example-com-tls
```

**Ingress.** Add `cert-manager.io/cluster-issuer: letsencrypt` to the Ingress
annotations.

### Solver config reference

| Field | Default | Description |
|---|---|---|
| `apiTokenSecretRef.name` | (required) | Secret holding the Hostinger API token |
| `apiTokenSecretRef.key` | `api-token` | Key in that Secret |
| `ttl` | `60` | TTL of the challenge TXT record, in seconds |
| `zone` | resolved zone | Hostinger domain to edit, when it differs from the zone cert-manager resolves through SOA lookups (e.g. delegated subzones) |
| `serverURL` | `https://developers.hostinger.com` | Hostinger API endpoint |

## Renewal

cert-manager tracks each certificate's expiry and renews it on its own, by default
when two-thirds of its lifetime has passed (configurable with `renewBefore` or
`renewBeforePercentage`). Each renewal solves a fresh DNS01 challenge through this
webhook. You don't need to do anything. The e2e suite checks this by issuing
1-hour certificates and waiting for cert-manager to renew both a `Certificate` and
a Gateway certificate.

## Chart values

| Value | Default | Description |
|---|---|---|
| `groupName` | `acme.hostinger.taha2samy-3.github.io` | API group; must match the issuer's `groupName` |
| `certManager.namespace` / `.serviceAccountName` | `cert-manager` | cert-manager controller identity allowed to call the webhook |
| `secretAccess.namespaces` | `[]` | Extra namespaces whose Secrets the webhook may `get` (the release namespace is always included) |
| `secretAccess.resourceNames` | `[]` | Restrict Secret access to these names |
| `image.repository` / `.tag` / `.digest` | release image, pinned by digest | Image to run |
| `replicaCount` | `1` | Use `2` with `podDisruptionBudget.enabled=true` for HA |
| `logLevel` | `2` | klog verbosity |
| `resources` | 10m / 64Mi requests, 256Mi limit | |
| `networkPolicy.enabled` | `false` | Restrict ingress to the webhook port and egress to DNS + HTTPS |
| `serviceMonitor.enabled` | `false` | Prometheus Operator ServiceMonitor for `/metrics` |
| `podSecurityContext`, `securityContext` | non-root, read-only rootfs, no capabilities, `RuntimeDefault` seccomp | |

See [values.yaml](https://github.com/taha2samy-3/cert-manager-webhook-hostinger/blob/main/deploy/cert-manager-webhook-hostinger/values.yaml) for everything else.

## Verify what you run

Every release is built by a dedicated reusable workflow and attested with GitHub
artifact attestations (SLSA v1 provenance, Build Level 3) plus a CycloneDX SBOM.
It is also signed with cosign (keyless):

```sh
gh attestation verify oci://ghcr.io/taha2samy-3/cert-manager-webhook-hostinger:<version> \
  --owner taha2samy-3 \
  --signer-workflow taha2samy-3/cert-manager-webhook-hostinger/.github/workflows/build-image.yml

gh attestation verify oci://ghcr.io/taha2samy-3/charts/cert-manager-webhook-hostinger:<version> \
  --owner taha2samy-3 \
  --signer-workflow taha2samy-3/cert-manager-webhook-hostinger/.github/workflows/build-chart.yml

cosign verify ghcr.io/taha2samy-3/cert-manager-webhook-hostinger:<version> \
  --certificate-identity-regexp '^https://github.com/taha2samy-3/cert-manager-webhook-hostinger/\.github/workflows/build-image\.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

The chart pins the image by digest, so a chart version always deploys the exact
image it was released with. The base images' own provenance is verified before
every build.

## Troubleshooting

```sh
kubectl get challenges -A
kubectl describe challenge <name> -n <namespace>
kubectl -n cert-manager logs deploy/hostinger-webhook-cert-manager-webhook-hostinger
```

| Symptom | Cause |
|---|---|
| `secrets "…" is forbidden` | The Secret's namespace is not in `secretAccess.namespaces` |
| `zone "…" not found in the Hostinger account` | The token's account doesn't manage that domain, or set `zone` |
| `hostinger API returned 401` | Token is wrong or expired |
| `429` in logs | Hostinger rate limit (90 req/min). The webhook backs off and retries |
| Challenge stuck "waiting for DNS propagation" | Check `dig TXT _acme-challenge.<domain> @ns1.dns-parking.com` |

## Migrating from Lokinado/cert-manager-webhook-hostinger

The solver config keys (`apiTokenSecretRef`, `serverURL`) are compatible. Install
this chart, then update `groupName` in your issuers to the new value (or set the
chart's `groupName` to your old one after uninstalling the old chart).

## Development

```sh
make test              # unit tests (race detector)
make test-conformance  # cert-manager DNS01 conformance suite (strict) against a fake Hostinger
make lint              # go vet, golangci-lint, helm lint
test/e2e/run.sh        # kind + cert-manager + Pebble: issuance, Gateway, auto-renewal
```

To run the conformance suite against the real API, export
`TEST_ZONE_NAME=example.com.` and `HOSTINGER_API_TOKEN=...`.

### Automation

| Workflow | What it does |
|---|---|
| `ci.yml` | Lint, unit + conformance tests, govulncheck, multi-arch image build + scan, e2e on two Kubernetes versions |
| `release.yml` | On `v*` tags: CI, then the SLSA L3 builders `build-image.yml` and `build-chart.yml`, then the GitHub release |
| Dependabot + `dependabot-auto-merge.yml` | Go modules, base image digests and actions; patch/minor merged automatically after CI passes |
| `auto-release.yml` | Weekly patch release when shipped files changed, so updates reach users |
| `upstream-watch.yml` | Weekly Hostinger API drift check (opens an issue) and scan of the published image |
| `scorecard.yml` | CodeQL and OpenSSF Scorecard |

## License

Apache-2.0
