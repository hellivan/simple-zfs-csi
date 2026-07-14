# zfs-shares Makefile

REGISTRY ?= ghcr.io/hellivan
TAG      ?= latest
NFS_IMG      ?= $(REGISTRY)/zfs-shares-nfs:$(TAG)
NVMEOF_IMG   ?= $(REGISTRY)/zfs-shares-nvmeof:$(TAG)

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

## Regenerate DeepCopy code from the Go types.
## The CRD and RBAC manifests are hand-maintained in the Helm chart
## (charts/zfs-shares/templates/) — the single source of truth.
.PHONY: manifests
manifests:
	go run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION) \
		object:headerFile= paths=./api/...

.PHONY: docker-nfs
docker-nfs:
	docker build -f build/nfs.Dockerfile -t $(NFS_IMG) .

.PHONY: docker-nvmeof
docker-nvmeof:
	docker build -f build/nvmeof.Dockerfile -t $(NVMEOF_IMG) .

.PHONY: docker
docker: docker-nfs docker-nvmeof

.PHONY: docker-push
docker-push: docker
	docker push $(NFS_IMG)
	docker push $(NVMEOF_IMG)

## Install just the CRD (rendered from the chart, the single source of truth).
.PHONY: install-crd
install-crd:
	helm template zfs-shares $(CHART_DIR) -s templates/crd.yaml | kubectl apply -f -

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

## Uninstall the chart (the CRD is retained via helm.sh/resource-policy: keep).
.PHONY: helm-uninstall
helm-uninstall:
	helm uninstall zfs-shares --namespace zfs-shares

