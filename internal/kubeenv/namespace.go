// Package kubeenv resolves facts about the Kubernetes environment a binary is
// running in, so every command derives them identically instead of each doing
// its own slightly different thing.
package kubeenv

import (
	"os"
	"strings"
)

// inClusterNamespacePath is the file every pod mounts with its own namespace.
// It is the last resort for Namespace, and is exactly the fallback
// controller-runtime uses when LeaderElectionNamespace is unset
// (pkg/leaderelection/leader_election.go), so a --namespace flag resolved
// through here behaves the same as --leader-election-namespace next to it.
const inClusterNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// Namespace determines the namespace a binary runs in, in descending order of
// explicitness: an explicit value (typically a flag that already defaults to
// $POD_NAMESPACE), then the pod's service-account namespace file.
//
// It returns "" only when the namespace is genuinely undeterminable — running
// outside a pod with nothing passed — which callers that need it should treat
// as fatal rather than proceeding with an empty namespace: an empty namespace
// does not fail cleanly at the API, it produces requests that come back
// NotFound and are then mistaken for "not configured".
func Namespace(explicit string) string {
	if explicit != "" {
		return explicit
	}
	b, err := os.ReadFile(inClusterNamespacePath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
