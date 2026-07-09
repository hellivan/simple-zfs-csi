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

## Regenerate DeepCopy code and the CRD manifest from the Go types.
.PHONY: manifests
manifests:
	go run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION) \
		object:headerFile= paths=./api/...
	go run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION) \
		crd rbac:roleName=zfs-shares-controller paths=./api/... paths=./internal/controller/... \
		output:crd:dir=config/crd output:rbac:dir=config/rbac

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

## Install just the CRD.
.PHONY: install-crd
install-crd:
	kubectl apply -f config/crd/zfsshares.storage.zfs-shares.io.yaml

## Deploy CRD, RBAC and both DaemonSets.
.PHONY: deploy
deploy: install-crd
	kubectl apply -f deploy/

.PHONY: undeploy
undeploy:
	kubectl delete -f deploy/ --ignore-not-found
	kubectl delete -f config/crd/zfsshares.storage.zfs-shares.io.yaml --ignore-not-found

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
