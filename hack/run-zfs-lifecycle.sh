#!/bin/sh
# Runs the lifecycle scenarios (internal/controller/lifecycle_test.go) with the real
# reconcilers against the real pool, through the toolbox pod (its `zfs` runs on the
# host). It never talks to Kubernetes and only touches datasets under
# ZFS_CONF_ROOT, which must contain "verify_zfs_functions"; every zfs call is
# checked against that root before it runs.
#
#   KUBE_CONTEXT=admin@kube-sl-home hack/run-zfs-lifecycle.sh [go test -run pattern]
set -eu
ctx="${KUBE_CONTEXT:-admin@kube-sl-home}"
ns="${NAMESPACE:-simple-zfs-csi}"
root="${ZFS_CONF_ROOT:-spinning-archive/verify_zfs_functions}"
run="${1:-TestLifecycle}"
bin="$(mktemp)"
trap 'rm -f "$bin"' EXIT
cd "$(dirname "$0")/.."
CGO_ENABLED=0 go test -c -tags zfsreal -o "$bin" ./internal/controller
pod="$(kubectl --context "$ctx" -n "$ns" get pod -l app.kubernetes.io/component=toolbox -o jsonpath='{.items[0].metadata.name}')"
kubectl --context "$ctx" -n "$ns" cp "$bin" "$pod:/tmp/controller.test"
kubectl --context "$ctx" -n "$ns" exec "$pod" -- sh -c \
  "chmod +x /tmp/controller.test && ZFS_CONF_ROOT='$root' LIFECYCLE_WALKS='${LIFECYCLE_WALKS:-6}' LIFECYCLE_MAX_CREATES='${LIFECYCLE_MAX_CREATES:-}' /tmp/controller.test -test.v -test.timeout 40m -test.run '$run'; rc=\$?; rm -f /tmp/controller.test; exit \$rc"
