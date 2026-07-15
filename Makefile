# zfs-shares Makefile

REGISTRY ?= ghcr.io/hellivan
TAG      ?= latest
NFS_IMG      ?= $(REGISTRY)/zfs-shares-nfs:$(TAG)
NVMEOF_IMG   ?= $(REGISTRY)/zfs-shares-nvmeof:$(TAG)
DISCOVERY_IMG ?= $(REGISTRY)/zfs-shares-discovery:$(TAG)
WATCHER_IMG   ?= $(REGISTRY)/zfs-shares-watcher:$(TAG)

CONTROLLER_GEN_VERSION ?= v0.16.5
CHART_DIR ?= charts/zfs-shares

.PHONY: all
all: build

.PHONY: build
build:
	go build ./...

.PHONY: test
test:
	go test ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: tidy
tidy:
	go mod tidy

## Regenerate DeepCopy code and the CRD from the Go types. The generated CRD is
## written to the chart's Helm-native crds/ directory (the single source of
## truth); the API types + kubebuilder markers are authoritative.
.PHONY: manifests
manifests:
	go run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION) \
		object:headerFile= paths=./api/... \
		crd output:crd:dir=$(CHART_DIR)/crds

.PHONY: docker-nfs
docker-nfs:
	docker build -f build/nfs.Dockerfile -t $(NFS_IMG) .

.PHONY: docker-nvmeof
docker-nvmeof:
	docker build -f build/nvmeof.Dockerfile -t $(NVMEOF_IMG) .

.PHONY: docker-discovery
docker-discovery:
	docker build -f build/discovery.Dockerfile -t $(DISCOVERY_IMG) .

.PHONY: docker-watcher
docker-watcher:
	docker build -f build/watcher.Dockerfile -t $(WATCHER_IMG) .

.PHONY: docker
docker: docker-nfs docker-nvmeof docker-discovery docker-watcher

.PHONY: docker-push
docker-push: docker
	docker push $(NFS_IMG)
	docker push $(NVMEOF_IMG)
	docker push $(DISCOVERY_IMG)
	docker push $(WATCHER_IMG)

## Install just the CRD from the chart's Helm-native crds/ directory. Use this to
## roll a schema change, since `helm upgrade` never updates crds/ resources.
.PHONY: install-crd
install-crd:
	kubectl apply -f $(CHART_DIR)/crds/

## --- Helm ---
.PHONY: helm-lint
helm-lint:
	helm lint $(CHART_DIR)

.PHONY: helm-template
helm-template:
	helm template rel $(CHART_DIR) --namespace zfs-shares

.PHONY: helm-package
helm-package:
	helm package $(CHART_DIR) --version $(TAG) --app-version $(TAG) --destination dist

## Install the chart into the current kube-context.
.PHONY: helm-install
helm-install:
	helm upgrade --install zfs-shares $(CHART_DIR) \
		--namespace zfs-shares --create-namespace

## Uninstall the chart. Helm never deletes CRDs installed from crds/, so the
## ZfsShare CRD (and any ZfsShare objects) are retained.
.PHONY: helm-uninstall
helm-uninstall:
	helm uninstall zfs-shares --namespace zfs-shares

