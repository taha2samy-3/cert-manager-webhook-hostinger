GO ?= go
OS ?= $(shell $(GO) env GOOS)
ARCH ?= $(shell $(GO) env GOARCH)

IMAGE_NAME ?= ghcr.io/taha2samy-3/cert-manager-webhook-hostinger
IMAGE_TAG ?= dev
CHART_DIR := deploy/cert-manager-webhook-hostinger

LOCALBIN ?= $(CURDIR)/bin
OUT := $(CURDIR)/_out

gomodver = $(shell $(GO) list -m -f '{{.Version}}' $(1) 2>/dev/null)

# setup-envtest is versioned like controller-runtime's release branch and the
# envtest Kubernetes version follows k8s.io/api, both read from go.mod.
ENVTEST ?= $(LOCALBIN)/setup-envtest
ENVTEST_VERSION ?= $(shell printf '%s' '$(call gomodver,sigs.k8s.io/controller-runtime)' | sed -E 's/^v?([0-9]+)\.([0-9]+).*/release-\1.\2/')
ENVTEST_K8S_VERSION ?= $(shell printf '%s' '$(call gomodver,k8s.io/api)' | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

.PHONY: all
all: lint test

.PHONY: build
build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '-s -w' -o $(OUT)/webhook ./cmd/webhook

.PHONY: test
test:
	$(GO) test -race -count=1 ./...

# Runs cert-manager's DNS01 conformance suite. Uses a fake Hostinger API unless
# TEST_ZONE_NAME and HOSTINGER_API_TOKEN are exported.
.PHONY: test-conformance
test-conformance: setup-envtest
	KUBEBUILDER_ASSETS="$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" \
	TEST_ASSET_ETCD="$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)/etcd" \
	TEST_ASSET_KUBE_APISERVER="$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)/kube-apiserver" \
	TEST_ASSET_KUBECTL="$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)/kubectl" \
	$(GO) test -tags conformance -count=1 -v -run TestConformance .

.PHONY: lint
lint:
	$(GO) vet ./...
	$(GO) vet -tags conformance .
	golangci-lint run ./...
	helm lint --strict $(CHART_DIR)

.PHONY: image
image:
	docker build --build-arg VERSION=$(IMAGE_TAG) -t $(IMAGE_NAME):$(IMAGE_TAG) .

.PHONY: rendered-manifest.yaml
rendered-manifest.yaml: | $(OUT)
	helm template cert-manager-webhook-hostinger $(CHART_DIR) \
	  --namespace cert-manager \
	  --set image.repository=$(IMAGE_NAME) \
	  --set image.tag=$(IMAGE_TAG) > $(OUT)/rendered-manifest.yaml

.PHONY: setup-envtest
setup-envtest: $(ENVTEST)
	$(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path >/dev/null

$(ENVTEST): | $(LOCALBIN)
	GOBIN=$(LOCALBIN) $(GO) install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(ENVTEST_VERSION)

$(LOCALBIN) $(OUT):
	mkdir -p $@

.PHONY: clean
clean:
	chmod -R u+w $(LOCALBIN) 2>/dev/null || true
	rm -rf $(LOCALBIN) $(OUT)
