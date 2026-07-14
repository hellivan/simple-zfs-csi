# Deployment & CRD management — decisions and future improvements

Notes captured while consolidating the deployment story onto the Helm chart.
This is a living document: the "Decisions" section records what we settled on
and why; the "Future improvements" section tracks options we deliberately did
*not* implement yet.

## Current state (as of this writing)

- Single deploy path: the Helm chart at `charts/zfs-shares/`.
- The CRD lives in exactly one place: `charts/zfs-shares/templates/crd.yaml`
  (templated, gated by `crds.install` / `crds.keep`).
- `make install-crd` renders the CRD out of the chart
  (`helm template ... -s templates/crd.yaml | kubectl apply -f -`) so the plain
  `kubectl` path can never diverge from the chart.
- `make manifests` only regenerates deepcopy code; the CRD/RBAC are
  hand-maintained in the chart.

## Decisions

### D1 — Remove the raw `deploy/` manifests; the chart is the single deploy source

- **What:** Deleted `deploy/` (namespace, RBAC, both DaemonSets) and the
  Makefile `deploy`/`undeploy` targets. Added `helm-uninstall`.
- **Why:** The raw manifests duplicated what the chart already renders and had
  already drifted — the per-controller metric-port-collision fix landed in the
  chart but was never propagated to `deploy/`. Duplication = drift = a real bug
  source. One source of truth removes the whole class of error.

### D2 — CRD is a single hand-maintained copy inside the chart (not duplicated in `config/crd/`)

- **What:** Deleted `config/crd/`. The chart template is now the only CRD copy.
- **Why:** `config/crd/…yaml` and `templates/crd.yaml` were byte-identical apart
  from the Helm wrapper. The `const:`→`enum:` / `required: [nvmeof]` /
  conditions list-map-keys fix previously had to be applied to *both* — exactly
  the drift we want to eliminate.
- **Why the templated chart copy (and not a plain file / Helm `crds/` dir) as
  the survivor:** it preserves the documented `crds.install` / `crds.keep`
  toggles and lets `helm upgrade` roll CRD schema changes. A plain file or the
  Helm-native `crds/` directory cannot do either.
- **Accepted trade-off:** the CRD stays hand-authored. `controller-gen` emits a
  *plain* CRD, so it can't target the templated file directly. Since we edit the
  schema rarely and by hand anyway, this is acceptable for now (see F1).

## CRD-management patterns (survey, for reference)

How other projects ship CRDs, and the trade-offs:

1. **Helm-native `crds/` directory** — plain YAML in `charts/x/crds/`. Installed
   before templates, never templated, and **never upgraded or deleted** by Helm.
   Simple; no toggle, no upgrade path.
2. **CRDs as templates gated by a values flag** *(what we do)* — e.g.
   cert-manager's `crds.enabled` + `crds.keep` with a
   `helm.sh/resource-policy: keep` annotation. Gives toggles and `helm upgrade`
   schema rollout; you take responsibility for not breaking existing objects.
3. **Separate CRD chart / subchart** — e.g. kube-prometheus-stack's
   `prometheus-operator-crds`; ArgoCD/Flux/Istio decouple CRDs from the workload
   release. Driven by independent lifecycle and the 262 KB
   `last-applied-configuration` annotation limit for very large CRDs.
4. **Runtime reconciliation by the operator** — e.g. **Cilium**. CRDs are
   generated from Go types, committed, and `//go:embed`-ed into the binary; on
   startup the operator runs an idempotent `CreateUpdateCRD` that also *upgrades*
   the schema (guarded by a stamped schema-version label). The Helm chart ships
   **no** CRD manifests; it only exposes `operator.skipCRDCreation` (opt out) and
   `crdWaitTimeout` (agent exits if CRDs never appear). This is the fullest
   "generated single source of truth": one actor generates at build and
   applies+upgrades at runtime, sidestepping all Helm CRD limitations.

## Future improvements (not yet implemented)

### F1 — Optionally drive the CRD from the Go types (generated pipeline)

If the schema starts churning, move to a generate-and-assemble pipeline:

- Add kubebuilder markers / CEL `+kubebuilder:validation:XValidation` to express
  the protocol discriminator (`oneOf` nfs/nvmeof, the `required:` blocks) that is
  currently hand-authored.
- Have `make manifests` emit the CRD, then either (a) ship it via
  `charts/zfs-shares/crds/` (loses `crds.install`/`crds.keep`), or (b) script a
  copy of the generated body into `templates/crd.yaml` (keeps the toggles).
- Add a CI drift-check: run `make manifests` and fail if the tree is dirty. A
  generator without this check still drifts.
- **Cost/benefit:** worth it only if the schema changes often. Today it's small
  and stable, so the hand-maintained chart copy (D2) wins.

### F2 — (Considered, rejected for now) Cilium-style runtime CRD reconciliation

Embed the CRD in the controller binary and apply/upgrade it on startup.

- **Why attractive:** ultimate single source of truth; no chart/CRD drift;
  schema upgrades ride with the image; avoids Helm CRD limitations.
- **Why rejected for this project:** our controllers are **per-node DaemonSets**
  with no leader election, unlike Cilium's single cluster-scoped operator. Every
  node's pod would race to create/update the same cluster-scoped CRD (idempotent
  and safe, but redundant apiserver writes and muddy ownership), and it requires
  granting node pods **cluster-wide `customresourcedefinitions` create/update
  RBAC** — a much broader grant than a node-scoped share controller needs.
- **If ever adopted:** give CRD management to a single coordinating component (or
  gate behind leader election / a one-shot Job), embed the generated CRD, and
  keep a `skipCRDCreation`-style escape hatch + a `crdWaitTimeout` contract,
  mirroring Cilium.

### F3 — Revisit `config/rbac` generation

`make manifests` previously targeted `config/rbac` (which doesn't exist); RBAC is
hand-maintained in `charts/zfs-shares/templates/rbac.yaml`. If F1 is adopted,
decide whether RBAC should also be generated from the `+kubebuilder:rbac`
markers and assembled into the chart, or remain hand-authored.
