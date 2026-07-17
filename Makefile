# simple-zfs-csi Makefile

REGISTRY ?= ghcr.io/hellivan
TAG      ?= latest
NFS_IMG      ?= $(REGISTRY)/simple-zfs-csi-nfs:$(TAG)
NVMEOF_IMG   ?= $(REGISTRY)/simple-zfs-csi-nvmeof:$(TAG)
DISCOVERY_IMG ?= $(REGISTRY)/simple-zfs-csi-discovery:$(TAG)
OPERATOR_IMG  ?= $(REGISTRY)/simple-zfs-csi-operator:$(TAG)
CSI_CONTROLLER_IMG ?= $(REGISTRY)/simple-zfs-csi-controller:$(TAG)
CSI_NODE_IMG ?= $(REGISTRY)/simple-zfs-csi-node:$(TAG)

CONTROLLER_GEN_VERSION ?= v0.16.5
CHART_DIR ?= charts/simple-zfs-csi

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

.PHONY: docker-operator
docker-operator:
	docker build -f build/operator.Dockerfile -t $(OPERATOR_IMG) .

.PHONY: docker-csi-controller
docker-csi-controller:
	docker build -f build/csi-controller.Dockerfile -t $(CSI_CONTROLLER_IMG) .

.PHONY: docker-csi-node
docker-csi-node:
	docker build -f build/csi-node.Dockerfile -t $(CSI_NODE_IMG) .

.PHONY: docker
docker: docker-nfs docker-nvmeof docker-discovery docker-operator docker-csi-controller docker-csi-node

.PHONY: docker-push
docker-push: docker
	docker push $(NFS_IMG)
	docker push $(NVMEOF_IMG)
	docker push $(DISCOVERY_IMG)
	docker push $(OPERATOR_IMG)
	docker push $(CSI_CONTROLLER_IMG)
	docker push $(CSI_NODE_IMG)

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
	helm template rel $(CHART_DIR) --namespace simple-zfs-csi

.PHONY: helm-package
helm-package:
	helm package $(CHART_DIR) --version $(TAG) --app-version $(TAG) --destination dist

## Install the chart into the current kube-context.
.PHONY: helm-install
helm-install:
	helm upgrade --install simple-zfs-csi $(CHART_DIR) \
		--namespace simple-zfs-csi --create-namespace

## Uninstall the chart. Helm never deletes CRDs installed from crds/, so the
## NetworkExport CRD (and any NetworkExport objects) are retained.
.PHONY: helm-uninstall
helm-uninstall:
	helm uninstall simple-zfs-csi --namespace simple-zfs-csi

