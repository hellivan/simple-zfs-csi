# Design Decisions

An append-only log of architectural decisions (ADR-lite). Each entry records the
decision, the context, the options weighed, and the consequences. Newest first.

The complementary conventions doc is [api-conventions.md](api-conventions.md);
the build plan is [implementation-strategy.md](implementation-strategy.md).

---

## ADR-0012 — Pool maintenance: operator-reconciled scrub CronJobs

**Status:** Accepted (2026-07-18) · **Scope:** new `cmd/zpool-scrub` (bundled in the discovery image), operator `ScrubReconciler` + config ConfigMap, `ZfsPool` watch, Helm values + RBAC · **Builds on** the `ZfsPool` discovery/takeover model (ADR-0003).

### Context

ZFS pools need periodic `zpool scrub` (read-verify + repair-from-redundancy) as
routine maintenance. Requirements: configure it in `values.yaml`; surface each run
as a Kubernetes Job that succeeds or fails (so kube-state-metrics → Prometheus
alerting is trivial); run on the node that currently imports the pool; and follow
the pool automatically when it moves nodes (takeover).

The hard part is not the scrub — it is **node targeting**. A `zpool scrub` must run
where the pool is imported, but a `CronJob`'s pod template is static while a pool
can move. A plain chart-rendered CronJob would need a node-label indirection to
track the host. Instead we let the operator — which already watches `ZfsPool` and
knows `status.currentNode` — own the CronJob and re-target it.

### Decisions

1. **Config: an explicit per-pool list in values, rendered into an operator
   ConfigMap.** `maintenance.scrub.pools: [{ guid, schedule }]` (plus a top-level
   `enabled` and default schedule) is rendered into a ConfigMap the operator reads
   via `--scrub-config-file` (the operator's first config file; it was flags-only).
   Explicit list — not auto-all-pools — so the admin controls exactly which pools
   are scrubbed and at what cadence.

2. **The operator reconciles one CronJob per configured pool.** A `ScrubReconciler`
   in the operator ensures a `CronJob` named `scrub-<guid>` whose `jobTemplate` pins
   the pod via `nodeAffinity` on `kubernetes.io/hostname == ZfsPool.status.currentNode`
   (affinity, not raw `nodeName`, so node taints/tolerations still apply). It watches
   `ZfsPool` (re-pin on takeover) and the config ConfigMap (re-render on change).
   When the pool is `NODE_OFFLINE` or has no current node it sets the CronJob
   `.spec.suspend: true` rather than scheduling a doomed scrub. CronJobs carry a
   `pool-guid` label; the reconciler prunes those whose pool left the config or
   disappeared (a cluster-scoped `ZfsPool` cannot own a namespaced CronJob via
   ownerRef GC, so pruning is label-based).

3. **The scrub itself is a small host-exec binary, reusing the discovery image.**
   `cmd/zpool-scrub` resolves the pool GUID → name, runs `zpool scrub -w <pool>`
   (blocking), then parses `zpool status` and **exits non-zero on unrepairable
   errors / an unhealthy pool, zero on a clean scrub** — so the Job's success/failure
   is the scrub result. It reuses the discovery plane's `zpool.HostExec` (chroot/
   nsenter) and is bundled into the existing discovery image (no new image or CI
   matrix entry); the CronJob just overrides the container command. CronJobs use
   `concurrencyPolicy: Forbid` and `backoffLimit: 0` (a failed scrub is a signal, not
   a transient error to retry) and a long `activeDeadlineSeconds`.

4. **Observability via native Job status.** kube-state-metrics
   (`kube_job_status_failed`, `kube_cronjob_status_last_schedule_time`, …) is the
   monitoring surface — no bespoke exporter. Writing scrub results back to
   `ZfsPool.status` (last scrub time, repaired bytes, errors) is a deferred
   enhancement.

5. **Extensible to other maintenance later.** The same reconciler shape covers
   `zpool trim` (SSD maintenance) as a sibling task; this cut ships scrub only.

### Consequences

- The operator gains `batch/cronjobs` RBAC (namespaced) and a config ConfigMap; no
  new CRD, no new image (scrub rides the discovery image).
- Native CronJob scheduling + native Job pass/fail keep the Prometheus story simple
  and keep scheduling logic out of Go (the operator only reconciles the CronJob spec,
  not the cron ticks).
- Self-healing on takeover: the operator rewrites the affinity when a pool moves.
- A pool not listed in `maintenance.scrub.pools` is never scrubbed by the driver —
  intentional; the admin opts each pool in.

### Plan (→ [implementation-strategy.md](implementation-strategy.md) Step 13)

1. `internal/zpool`: `Scrub` + status parse (host-exec).
2. `cmd/zpool-scrub` binary; bundle it in the discovery image.
3. Operator `ScrubReconciler`: load config file, reconcile one CronJob per pool,
   pin via nodeAffinity, suspend-when-offline, label-prune.
4. Chart: `maintenance.scrub` values → operator ConfigMap; operator `cronjobs` RBAC;
   mount the config file.
5. Verify: unit tests (status parse → exit code, CronJob render/pin/suspend/prune),
   `helm template`; live scrub Job is the manual e2e.

---

## ADR-0011 — NVMe-oF zero-trust: per-attach host NQN + DH-CHAP

**Status:** Accepted (2026-07-18) · **Scope:** csi-node (authenticated connect), operator (attach reconciler), nvmeof aggregator + nvmet, `NetworkExport.nvmeof` spec, per-attach `Secret`, Helm · **Extends** ADR-0010; **completes** ADR-0005 for NVMe-oF.

### Context

ADR-0010 made NFS zero-trust by default (temporal + node-IP allow-list) but left
NVMe-oF **temporal-only**: the subsystem exists only while a node is attached, yet
`attr_allow_any_host=1` lets *any* initiator on the storage network connect, and
there is no in-band authentication. The goal is parity — NVMe-oF restricted to the
authorized consumer **and** password-authenticated with DH-CHAP, on by default.

Two facts shape the design. (a) The CSI attach call carries only the node *name*,
not an NVMe host NQN or key. (b) In nvmet the DH-CHAP key is an attribute of the
**host object** (`hosts/<nqn>/dhchap_key`) and `allowed_hosts` entries are symlinks
to it, so the key is **per host NQN**, and DH-CHAP is only enforced with
`attr_allow_any_host=0` + an explicit host object. A single node attaching several
volumes at once would, under one stable per-node NQN, share one key across all of
them — so rotating a key on a new attach would clobber sibling connections.

### Decisions

1. **Per-attach host NQN, derived — not published, not secret.** The effective host
   NQN is derived deterministically from `(nodeName, volumeID)` (a UUIDv5 →
   `nqn.2014-08.org.nvmexpress:uuid:<uuid>`). Both sides compute it independently —
   the **operator** from the attach request's node + volume, the **node** from its
   own name + the `volumeID` — so **nothing is published**: no `NvmeHost` CRD, and
   the node keeps writing *no* CRDs (ADR-0003 preserved). The node passes
   `--hostnqn=<derived>` **and a matching `--hostid=<uuid>`** (the same UUID) to
   `nvme connect`, so initiator and target agree and the NVMe spec's "one host id
   per NQN" rule is respected even though the node holds several distinct host NQNs
   at once (supported on the target kernel — verified on Talos 6.18). The NQN is an
   **identifier, not a credential** (it is derivable by anyone who knows the node
   and volume names).

2. **Per-attach — because the key is per host object.** Given fact (b), a unique
   host NQN per attach gives each attachment its **own** host object and therefore
   its own `dhchap_key`. This yields true per-attach key rotation (detach + reattach
   → new NQN → new key) with **no cross-impact** on a node's other live NVMe
   attachments. A stable per-node NQN could not.

3. **The NQN allow-list is the prerequisite for DH-CHAP, not a redundant layer.**
   nvmet enforces DH-CHAP only under `attr_allow_any_host=0` with an explicit host
   object carrying the key; the derived NQN is both the default-deny ACL *and* the
   configfs handle the key hangs off. So the two are complementary: NQN = identity
   (deterministic, nothing to distribute); the DH-CHAP key = the actual random
   secret. Making the NQN itself random would only add a second secret to ship for
   no gain.

4. **The DH-CHAP key is per-attach, operator-generated, and travels only in a
   `Secret`.** The operator generates a random key in the NVMe `DHHC-1` format,
   stores it in a Kubernetes `Secret` (owner-referenced by the `ZfsShareAttachRequest`
   for GC), and sets `NetworkExport.nvmeof.dhchapSecretRef`. The raw key never lands
   in a widely-readable CRD spec/status. Exactly two readers: the **storage-node
   nvmet aggregator** (writes it to the host object's `dhchap_key`) and the
   **consuming node** (passes it as `nvme connect --dhchap-secret`). Readiness gating
   (ADR-0010 §4) already guarantees the target is programmed before the node connects.
   The Secret's **data-key name is configurable** (operator flag
   `--nvmeof-dhchap-secret-key`, default `dhchap-key`) and is always recorded on the
   `NetworkExport` (`dhchapSecretKey`), so readers never assume a fixed key name.

5. **One-way DH-CHAP first; bidirectional later.** The initiator authenticates to
   the target (`dhchap_key`). Bidirectional (`dhchap_ctrl_key`, target authenticates
   back) is an opt-in follow-up, not in this cut.

6. **Enabled by default; degrades safely.** A chart flag
   `nvmeof.auth.dhchap.enabled` (default `true`) governs key generation and
   programming. NQN allow-listing is unconditional. With DH-CHAP off, NVMe-oF is
   still identity-restricted by the (guessable) NQN — weak but non-zero; DH-CHAP is
   what makes it a real boundary.

### Consequences

- NVMe-oF becomes zero-trust by default: default-deny by host NQN **and**
  password-authenticated, matching NFS's posture. Completes ADR-0005 for NVMe-oF.
- **No new CRD**, and the node stays CRD-free (ADR-0003 intact) — resolving toward
  per-attach rotation *removed* the `NvmeHost` moving part rather than adding it,
  because a per-attach NQN is derivable by both sides.
- New machinery: a shared host-NQN derivation helper (operator + node);
  `NetworkExport.nvmeof.{allowedHosts, dhchapSecretRef}`; operator key generation +
  `Secret` lifecycle; nvmet `dhchap_key` programming; node `--hostnqn` /
  `--dhchap-secret` connect flags; RBAC (operator: `secrets` create/delete; nvmeof
  aggregator + csi-node: `secrets` read).
- Talos-friendly: derived NQN needs no persisted host state; the key is ephemeral in
  a `Secret`.
- Exact `DHHC-1` key bytes (hash marker / optional CRC) are settled in code against a
  live target; unit tests mock the key. Live authenticated `nvme connect` is the
  manual e2e step.

### Plan (→ [implementation-strategy.md](implementation-strategy.md) Step 12)

1. Shared `HostNQN(nodeName, volumeID)` derivation helper (used by operator + node).
2. `NetworkExport.nvmeof.{allowedHosts, dhchapSecretRef}` fields.
3. Operator attach reconciler: set `allowedHosts` = derived NQN (default-deny); when
   DH-CHAP on, generate a per-attach key `Secret` (owner-ref the attach request) and
   set `dhchapSecretRef`.
4. nvmet: `attr_allow_any_host=0`, host object + `dhchap_key` from the `Secret`,
   link `allowed_hosts/<nqn>`.
5. csi-node: pass `--hostnqn` always; read the `Secret` and pass `--dhchap-secret`
   when a ref is set.
6. Chart: RBAC, `nvmeof.auth.dhchap.enabled`, wiring; `make manifests`.
7. Verify: unit tests (NQN derivation/default-deny, key gen + Secret lifecycle,
   nvmet dhchap programming, node connect flags); live authenticated `nvme connect`
   is manual.

---

## ADR-0010 — Attach-stage share lifecycle & zero-trust access control

**Status:** Accepted & implemented (2026-07-17) · **Scope:** CSI controller, csi-node (`CSIDriver`), operator, new `ZfsShareAttachRequest` CRD, `NetworkExport`/`ZfsShare` status · **Supersedes** ADR-0001 §2; **implements/extends** ADR-0005.

> **Implementation note.** Readiness is generation-gated using the existing
> `status.observedGeneration` fields rather than the mooted per-object
> `allowedClients` status: `ZfsShare` gained an `Exporting` phase and is marked
> `Bound` only once its child `NetworkExport` reports `Exported` for the current
> generation; the attach request is `Ready` only when its `ZfsShare` is `Bound`
> at `observedGeneration >= generation`. NVMe-oF host-NQN allow-listing is
> deferred (decision 3 ref-counting only exercises for NFS RWX), so an NVMe-oF
> share is temporal-only for now: it exists solely while its single consumer is
> attached, with allowed hosts left open.

### Context

ADR-0005 accepted "move access control to the attach stage" but kept the
provision-time share (ADR-0001 §2). Revisiting: a share created at `CreateVolume`
sits there exposed (or dormant-but-present) for the volume's whole life and only
gets *narrowed* when attaches arrive — the opposite of zero-trust. The intent is
that **nothing is exported until a specific node is authorized**, and the export
disappears again when the last consumer detaches. ADR-0001 §2's counter-argument
("shares work without a pod") is preserved by manual/static authoring, and its
stated reason for rejecting publish-time (node-side CRD writes needing RBAC) does
not apply here because the write happens **controller-side** at
`ControllerPublishVolume`. So ADR-0001 §2 is superseded; nothing breaks
technically.

### Decisions

1. **The share is lazy — created at attach, destroyed at last detach.**
   `CreateVolume` writes **only** the `ZfsDataset` (allocation). No `ZfsShare` at
   provision time. `DeleteVolume` deletes the `ZfsDataset` (and any stray
   `ZfsShare`, defensively). This supersedes ADR-0001 §2.

2. **Attach hook via `external-attacher`.** `CSIDriver.attachRequired: true` (both
   protocols — we use attach purely as the authorization hook, not for block
   map/lock). `ControllerPublishVolume(vol, node)` creates a
   `ZfsShareAttachRequest{vol, node}`; `ControllerUnpublishVolume` deletes it. The
   CSI controller stays thin: it only creates the request and polls its status.

3. **New `ZfsShareAttachRequest` CRD, aggregated declaratively in the operator.**
   One tiny object per `(volume, node)` attach. An operator reconciler aggregates
   the set per volume:
   - ≥1 request → **ensure `ZfsShare` exists**, `allowedClients` = resolve(each
     request's node) → compiles to `NetworkExport` as usual (ADR/THOUGHTS §7).
   - 0 requests → **delete the `ZfsShare`** (→ `NetworkExport` GC'd → export torn
     down).
   Ref-counting is free: the allow-list *is* the set of request objects. Each
   attach writes its **own** object (no read-modify-write contention on a shared
   allow-list field); a single leader-elected reconciler owns the `ZfsShare` write.

4. **Readiness bubbles up the chain; each reconciler reads only its neighbour.**
   `nfs`/`nvmeof` aggregator writes `NetworkExport.status` (applied allow-list +
   `observedGeneration`) → operator's `ZfsShare` reconciler reflects it into
   `ZfsShare.status` → operator's `ZfsShareAttachRequest` reconciler reflects
   "is my node applied?" into `ZfsShareAttachRequest.status` →
   `ControllerPublishVolume` polls **that** status before returning success (so the
   subsequent `NodePublish` mount finds a live export). Readiness is
   **generation-gated** (report the applied set and/or `observedGeneration`) so a
   stale "ready" from before this node was added can never satisfy the wait. This
   keeps RBAC minimal (the CSI controller reads only the request) and matches the
   existing owner/watch layering.

5. **NVMe-oF is single-node only.** `RWX`/multi-node access modes on a zvol +
   filesystem = corruption (ext4/xfs are not cluster filesystems). `CreateVolume`
   **and** `ValidateVolumeCapabilities` reject any `MULTI_NODE_*` access mode when
   `protocol == nvmeof` (`InvalidArgument`), sitting next to the existing
   `Block + nfs` rejection. NFS remains the only RWX path. Consequence: an NVMe-oF
   volume always has exactly one attach request, so its share lifecycle is the
   trivial create-on-attach / delete-on-detach case; the ref-counting in decision
   3 only ever exercises for NFS RWX.

6. **Static provisioning asymmetry (documented, not a blocker).** Kubernetes has
   **no in-tree NVMe-oF volume plugin** (only `nfs`, `iscsi`, `fc`). So the "bypass
   the driver with a native PV" path exists for **NFS** (`spec.nfs`) but **not**
   for NVMe-oF. Static NVMe-oF uses a **static CSI PV** (`spec.csi` + `volumeHandle`
   + `volumeAttributes = { poolGUID, dataset, protocol }`); the node plugin does the
   `nvme connect`. The admin pre-creates the zvol (and optionally the
   `NetworkExport`/`ZfsShare`, or lets the attach flow create it).

### Consequences

- **Zero-trust:** no export exists until a node is authorized; the export is torn
  down when the last consumer detaches. Truest reading of THOUGHTS §20.
- **CSI controller stays a thin adapter:** it creates `ZfsDataset` (+ on attach a
  `ZfsShareAttachRequest`) and polls status; **all** reconcile/aggregation lives in
  the `operator`.
- **New machinery required:** `external-attacher` sidecar + `VolumeAttachment` RBAC;
  `ControllerPublish/Unpublish`; the `ZfsShareAttachRequest` CRD + its reconciler;
  status subresources on `NetworkExport`/`ZfsShare`/`ZfsShareAttachRequest`;
  `attachRequired: true` on the `CSIDriver`.
- **Fencing unchanged:** `NODE_OFFLINE` remains the *availability* gate (ADR-0003);
  attach adds an *authorization* gate — orthogonal.
- **Crypto auth (DH-CHAP / TLS-PSK) and NQN discovery** (THOUGHTS §20.3, §21) layer
  on top of this attach stage later; out of scope for the first cut.

### Plan (→ [implementation-strategy.md](implementation-strategy.md) Step 11)

1. Forbid `nvmeof` multi-node access modes in `CreateVolume` + `ValidateVolumeCapabilities` (small, self-contained; ship first).
2. `ZfsShareAttachRequest` CRD + `allowedClients`/status fields on `ZfsShare`/`NetworkExport`.
3. Stop creating `ZfsShare` in `CreateVolume`; add `ControllerPublish/Unpublish` creating/deleting the request; wait on request status.
4. Operator: attach-request aggregation reconciler (lazy create/GC the `ZfsShare`) + status bubble-up.
5. Chart: `external-attacher` sidecar, `VolumeAttachment` RBAC, `CSIDriver.attachRequired: true`.
6. Verify: unit tests (nvmeof RWX rejection, aggregation create/GC, status gating); e2e RWO NVMe + RWX NFS attach/detach.

---

## ADR-0009 — Clone/restore: `spec.source` on `ZfsDataset`, same-pool `zfs clone`

**Status:** Accepted (2026-07-17) · **Scope:** Step 10 — API, agent, CSI controller, Helm RBAC

### Context

The last parity capability is provisioning a volume *from* a snapshot (restore) or
*from* another volume (clone). CSI expresses both through
`CreateVolume.VolumeContentSource` (`Snapshot` or `Volume`), resolved by
`external-provisioner` from a PVC `dataSource`. ZFS implements both with
`zfs clone`, which is copy-on-write and — critically — **same-pool only** (a clone
must live in the pool of its origin snapshot).

### Decisions

1. **Clone rides the existing `ZfsDataset` ownership boundary.** `ZfsDatasetSpec`
   grows an optional `source { snapshot | volume }` (a logical path relative to the
   pool root). The CSI controller sets it; the agent, on the hosting node, runs
   `zfs clone` instead of `zfs create`. No new CRD, no CSI-plane ZFS work — the
   agent stays the only ZFS actor, exactly like allocation, expansion and snapshot.

2. **From a snapshot → direct clone; from a volume → snapshot-then-clone.** For a
   `volume` source the agent first takes a deterministic intermediate snapshot
   (`<src>@clone-<newName>`, idempotent) and clones that, since `zfs clone` only
   consumes snapshots. Sizing is left to the existing `ensureSize` step: the clone
   inherits the origin's size, then converges to the requested capacity (grow
   only), so no size arg is passed at clone time and `volblocksize` stays inherited.

3. **Same-pool + same-type are enforced in the controller.** A clone across pools
   is impossible in ZFS, so `CreateVolume` rejects a source whose `poolGUID` differs
   from the (StorageClass-fixed) target pool with `InvalidArgument`. It likewise
   rejects a type mismatch (e.g. restoring a filesystem snapshot into an nvmeof
   zvol), deriving the source type from the source `ZfsDataset`. The controller
   echoes the `content_source` in the response and advertises `CLONE_VOLUME`.

4. **No new sidecar; only RBAC.** Restore/clone are built into
   `external-provisioner`; the chart just adds `volumesnapshots` read access
   (gated on `snapshotter.enabled`) so the provisioner can resolve a snapshot
   `dataSource` into a `VolumeContentSource`.

### Consequences

- Cross-pool restore/clone is intentionally unsupported (would need
  `zfs send/recv`, a heavier copy) — acceptable for the base driver; can be layered
  later behind the same `spec.source`.
- `DeleteVolume` still `zfs destroy -r`s the dataset; a clone's origin snapshot is
  independent (a snapshot's own `ZfsSnapshot`/finalizer governs it), and ZFS
  refuses to destroy an origin while clones exist — surfaced as a delete error and
  retried, rather than silently corrupting data.
- Verified by unit tests (agent clone from snapshot and from volume; controller
  restore/clone happy paths + cross-pool and type-mismatch rejection). Live
  `kubectl apply` of a PVC with a snapshot/PVC `dataSource` is the manual e2e step.
  With this, the base CSI feature set (provision, expand, snapshot, clone/restore)
  is complete.

---

## ADR-0008 — Snapshots: `ZfsSnapshot` CRD, agent-owned `zfs snapshot`

**Status:** Accepted (2026-07-17) · **Scope:** Step 9 — API, agent, CSI controller, Helm chart

### Context

Snapshots complete the democratic-csi-class base feature set (after expansion,
ADR-0004). CSI drives them through `external-snapshotter` →
`CreateSnapshot`/`DeleteSnapshot`, keyed on a source volume. The taxonomy decision
(ADR-0006) already fixed a *separate* `ZfsSnapshot` CRD rather than a third
`ZfsDataset` type, because a snapshot has its own lifecycle (derive-from-source,
read-only, restore/clone) and never carries an export or an expand path.

### Decisions

1. **`ZfsSnapshot` mirrors the `ZfsDataset` ownership boundary.** Spec is
   `{ poolGUID, dataset, snapshotName, sourceVolume }`; the CSI controller creates
   it, and the per-node **agent** that currently hosts the pool takes
   `zfs snapshot <poolName>/<dataset>@<snapshotName>` idempotently and destroys it
   via a finalizer (`storage.simple-zfs-csi.io/zfssnapshot`). No CSI-plane ZFS
   work — the agent stays the only ZFS actor, exactly like allocation.

2. **Status carries the CSI reply fields.** The agent reads them straight from
   ZFS: `readyToUse` (snapshot exists), `creationTime` (the `creation` property),
   `restoreSize` (the `referenced` bytes — the minimum size to restore). The
   controller's `CreateSnapshot` waits for `readyToUse` (like `CreateVolume` waits
   for `Ready`), then maps status → `csi.Snapshot`.

3. **The snapshot id is the object name.** `CreateSnapshot.Name` becomes both the
   `ZfsSnapshot` metadata.name and the ZFS `@` short name; `sourceVolume` (the CSI
   source volume id = source `ZfsDataset` name) is stored for `ListSnapshots`
   reporting and idempotency checks. `DeleteSnapshot` deletes the object; the
   finalizer drives `zfs destroy`. `ListSnapshots` supports id/source filters with
   offset-based pagination and advertises `LIST_SNAPSHOTS` alongside
   `CREATE_DELETE_SNAPSHOT`.

4. **Snapshot CRDs + controller are a cluster prerequisite.** The chart ships the
   `csi-snapshotter` sidecar (`csiController.snapshotter.*`, on by default), its
   `volumesnapshotcontents`/`volumesnapshotclasses` RBAC, and an optional
   `volumeSnapshotClasses` values map, but **not** the upstream snapshot CRDs or
   the `snapshot-controller` (installed once per cluster, like Ceph CSI docs).

### Consequences

- No new ZFS actor and no absolute paths cross the CSI boundary; snapshots reuse
  the same GUID→pool→node resolution and host-exec seam as allocation.
- Restore/clone (`VolumeContentSource` in `CreateVolume`) is **Step 10** and not
  yet implemented; `ZfsSnapshot` already carries enough (`poolGUID` + `dataset` +
  `snapshotName`) for a future `zfs clone` to consume.
- Verified by unit tests (zfs `Snapshot` idempotency, agent reconcile create/
  destroy/host-scoping, controller CreateSnapshot/DeleteSnapshot/ListSnapshots);
  live `VolumeSnapshot` create/restore is the manual e2e step.

---

## ADR-0007 — Project identity: renamed to `simple-zfs-csi`

**Status:** Accepted (2026-07-17) · **Scope:** module path, API group, CSI driver name, PVC annotation prefix, Helm chart, image names

### Context

The project began as `zfs-shares`, a name that described only the original plane
(network-sharing pre-provisioned ZFS over NFS/NVMe-oF via the `NetworkExport`
CRD). It has since grown a full CSI plane (dynamic provisioning, expansion, and
soon snapshots/clone). "zfs-shares" no longer describes what it is — a
self-contained **CSI driver** for ZFS — and the "simple" qualifier positions it
against the heavier alternatives it replaces (democratic-csi + TrueNAS, Ceph).

### Decisions

1. **Rename the project to `simple-zfs-csi`,** sweeping every name domain uniformly
   (`zfs-shares` → `simple-zfs-csi`):
   - Go module: `github.com/hellivan/simple-zfs-csi`
   - API group: `storage.simple-zfs-csi.io` (all four CRDs, finalizers,
     `LeaderElectionID`)
   - CSI driver name: `simple-zfs-csi.io` (`CSIDriver` object + StorageClass
     `provisioner` + `--driver-name` default)
   - PVC annotation prefix: `param.simple-zfs-csi.io/`
   - Helm chart: `charts/simple-zfs-csi`
   - Container images: `simple-zfs-csi-<component>`.

2. **Collapse the two CSI image names.** A uniform `<prefix>-<component>` scheme
   would yield `simple-zfs-csi-csi-controller` / `simple-zfs-csi-csi-node` (double
   `csi`), so those two use `simple-zfs-csi-controller` / `simple-zfs-csi-node`
   (the Helm image helper takes an explicit `suffix` of `controller`/`node`). The
   `cmd/` dirs and `build/*.Dockerfile` names stay `csi-controller`/`csi-node`
   (unambiguous internally).

### Consequences

- Breaking: the CRD API group and CSI driver name change, so this is not
  upgrade-compatible with any `zfs-shares` install — acceptable pre-1.0 (no
  production deployments).
- The on-disk repository directory is intentionally **not** renamed here (left to
  the maintainer to avoid breaking the active workspace path); it has no code
  impact.
- CRD manifests were regenerated (`make manifests`) so the group rename is
  authoritative in the generated YAML, not just hand-edited.

---

## ADR-0006 — CRD taxonomy: `ZfsDataset` (fs+zvol) vs a separate `ZfsSnapshot`

**Status:** Accepted (2026-07-17) · **Scope:** API types, CSI controller, agent

### Context

The allocation CRD was originally named `ZfsVolume` with `type: filesystem|volume`.
That overloads "volume": in CSI it means any PV, but in ZFS it means specifically
a *zvol*, so `ZfsVolume{type: volume}` reads as "a ZFS volume of type volume." It
also made the upcoming snapshot object look arbitrary — why does one CRD unify
filesystem+volume while a snapshot is a different CRD?

### Decisions

1. **Rename `ZfsVolume` → `ZfsDataset`** with `type: filesystem | volume`. "Dataset"
   is ZFS's own umbrella term ([api-conventions.md](api-conventions.md) §3), and it
   makes `type: volume` unambiguously a zvol. `shortName` changes `zvol` → `zds`.

2. **Group CRDs by lifecycle, not by ZFS taxonomy.** Taxonomically a snapshot *is*
   a kind of dataset, but `filesystem` and `volume` share one lifecycle (allocate →
   size/quota → share → expand → destroy), while a snapshot has a different one
   (derive-from-source → read-only → restore/clone, never shared/published). So the
   live allocation is `ZfsDataset`; a snapshot is a separate `ZfsSnapshot` (Step 9).

3. **The consumer model mirrors the `zfs` verbs:** `zfs create` a **dataset** →
   `zfs snapshot` it → `zfs clone` a snapshot into a new **dataset**. A snapshot
   never carries a filesystem/volume arm, an export, or an expand path, so folding
   it into `ZfsDataset` would mean an inert third `type` and `if type==snapshot`
   guards in every consumer — a separate CRD keeps each type's invariants clean.

### Consequences

- Breaking API rename (type, CRD `zfsdatasets`, finalizer
  `storage.simple-zfs-csi.io/zfsdataset`, CSI code, tests) — acceptable pre-1.0.
- `ZfsPool` stays as-is: Kubernetes names observed-infrastructure objects with
  plain nouns (`Node`, `CSINode`, `CSIStorageCapacity`); its empty spec already
  signals "discovered, not authored," so no rename is warranted.

---

## ADR-0005 — Access control and the CSI attach stage (direction)

**Status:** Accepted direction (2026-07-17), not yet implemented · **Scope:** CSIDriver, csi-controller, csi-node, `NetworkExport`

### Context

Today every share is effectively **public**: an NFS `NetworkExport` is exported
to the whole reachable network, and an NVMe-oF subsystem accepts any host NQN.
That is acceptable for the initial single-tenant bring-up but is **not** the end
state — we do not want any pod on any node able to mount any volume.

The driver currently sets `attachRequired: false` (ADR-0001/0003): there is no
controller-mediated attach step, because the node plugin does all reachability
work itself (`mount -t nfs` for NFS, `nvme connect` for NVMe-oF) and node-death
fencing comes from `ZfsPool.status.health == NODE_OFFLINE`, not from
`VolumeAttachment`. Ceph-RBD sets `attachRequired: true` for a genuine
map/lock/fence reason; our reason to (eventually) enable it is different but
compatible: **per-node access programming**.

The CSI attach stage — `ControllerPublishVolume(volume_id, node_id)` /
`ControllerUnpublishVolume`, tracked by `VolumeAttachment` objects and driven by
the `external-attacher` sidecar — is *controller-issued and node-parameterized*.
That is exactly the shape of "grant this specific node access to this volume,"
which is what access restriction needs.

### Decisions

1. **Keep `attachRequired: false` now.** No `ControllerPublishVolume`, no
   attacher, no `VolumeAttachment`. Simplicity while shares are trusted.

2. **NVMe-oF host allow-listing will move to the attach stage.** When we restrict
   NVMe-oF to specific consumers, we flip `attachRequired: true` and implement
   `ControllerPublishVolume` to add the consumer node's host NQN to the target
   subsystem's `allowed_hosts` (and `ControllerUnpublishVolume` to remove it),
   gated *before* `NodePublish`. This is the idiomatic CSI location for
   node-scoped access and gives us serialization + clean revoke for free.

3. **NFS allowed-clients live in the `NetworkExport` contract.** `NetworkExport`
   gains an allowed-clients field (NFS: CIDRs/IPs rendered into `/etc/exports`;
   NVMe-oF: host NQNs). Two ways to populate it, from coarse to fine:
   (a) **static policy** — allow the cluster node/pod CIDR, sourced from
   StorageClass/PVC params (simple, ship first); (b) **attach-driven** — the
   attach stage adds the specific consumer node's IP/NQN per publish (tightest,
   layered later). The executor (`nfs`/`nvmeof` controllers) stays generic; it
   only renders whatever allow-list the contract carries.

### Consequences

- Current simplicity is retained; access control is purely additive.
- Enabling attach later requires the `external-attacher` sidecar,
  `VolumeAttachment` RBAC, and `ControllerPublish/Unpublish` implementations —
  none of which exist today.
- `NetworkExport` grows an `allowedClients` field; it remains a ZFS-agnostic,
  node-local executor contract (an admin can still author one directly).
- Fencing semantics are unchanged: `NODE_OFFLINE` remains the availability gate;
  the attach stage would add an *authorization* gate, not replace fencing.

---

## ADR-0004 — Volume expansion: spec-driven size convergence, online grow

**Status:** Accepted (2026-07-17) · **Scope:** CSI controller + node, agent reconciler, Helm chart

### Context

democratic-csi-class parity starts with online volume expansion. A PVC edit that
requests more capacity flows through `external-resizer` →
`ControllerExpandVolume` → (for block) `NodeExpandVolume`. The backing size lives
in the `ZfsDataset` spec (`filesystem.quota` → ZFS `refquota`, `volume.size` → ZFS
`volsize`), which the per-node agent already owns. Expansion should reuse that
ownership rather than have the CSI plane touch ZFS directly.

### Decisions

1. **Expansion is spec convergence, not a special path.** `ControllerExpandVolume`
   only bumps the `ZfsDataset` spec size (retrying on conflict with the agent's
   status writes) and waits for the agent to observe it
   (`status.observedGeneration >= target`). The agent's reconciler gained an
   `ensureSize` step that runs on every reconcile: filesystem → `zfs set refquota`
   (grows or shrinks the cap), zvol → `zfs set volsize` (**grow only**, never
   shrink — shrinking a zvol under a live filesystem is unsafe). This also makes
   quota drift self-heal, not just explicit expands.

2. **`NodeExpansionRequired` follows the protocol.** NFS/filesystem quotas take
   effect the instant `refquota` is set, so no node work is needed
   (`NodeExpansionRequired: false`). A zvol grow only changes the target; the
   initiator must rescan the namespace and grow the on-device filesystem, so
   `NodeExpansionRequired: true`. `NodeExpandVolume` runs `nvme ns-rescan` then
   `resize2fs`/`xfs_growfs`; raw-block volumes stop after the rescan (no fs), and
   an NFS volume (no `NetworkExport` NQN) is a no-op.

3. **`volsize` alignment.** ZFS requires `volsize` to be a multiple of
   `volblocksize`, so the agent rounds the requested bytes up to the volume's
   block size (default 16 KiB) before `zfs set`.

4. **Online capability.** The controller Identity advertises
   `VOLUME_EXPANSION: ONLINE`; the controller service advertises `EXPAND_VOLUME`;
   the node service advertises `EXPAND_VOLUME`. Helm gains the `external-resizer`
   sidecar (`csiController.resizer.*`, on by default) plus RBAC for
   `persistentvolumeclaims/status` and `persistentvolumes` update. StorageClasses
   opt in per class with `allowVolumeExpansion: true`.

### Consequences

- No new CRD; expansion rides the existing `ZfsDataset` ownership boundary — the
  CSI plane stays a thin CRD adapter and only the agent runs ZFS.
- Shrinking is intentionally unsupported for zvols (and Kubernetes forbids PVC
  shrink anyway); filesystem `refquota` can still be lowered by editing the spec.
- Live `resize2fs`/`xfs_growfs` over NVMe-oF is the manual verification step (not
  unit-tested); unit tests cover the controller size-bump + node rescan/resize
  dispatch and the agent's `ensureSize` for both types.

---

## ADR-0003 — CSI node plugin: routing-only publish, NODE_OFFLINE fencing, protocol dispatch

**Status:** Accepted (2026-07-17) · **Scope:** Step 7 (`cmd/csi-node`), Helm chart

### Context

The node plugin is a privileged DaemonSet on every node. The controller
(ADR-0001) returns only a routing `volume_context = { poolGUID, dataset,
protocol }` — never an absolute path — so the node must resolve the real mount
target itself, at publish time, from live cluster state. It writes no CRDs.

### Decisions

1. **Routing resolved from `ZfsPool.status` at publish time.** `NodePublishVolume`
   loads the `ZfsPool` by `zpool.ResourceName(poolGUID)` (the same GUID→object
   mapping the operator uses) and reads `CurrentIP`, `BaseMountPath`, `PoolName`
   and `Health`. Resolving per-publish (not from a cached path) means pool
   takeover to a new node is picked up automatically on the next mount.

2. **NODE_OFFLINE fencing.** If `status.health == NODE_OFFLINE` (or there is no
   `CurrentIP`), publish fails `FailedPrecondition` with a clear message rather
   than mounting a stale/dead target. This is the node-side half of the watcher's
   fencing: the watcher marks the pool offline, the node refuses to mount it.

3. **`protocol` dispatches the publish mechanism; `volumeMode` is orthogonal.**
   - `nfs` → `mount -t nfs <CurrentIP>:<baseMountPath>/<dataset>` (filesystem
     only; block mode is rejected — mirrors the controller's rule).
   - `nvmeof` → `nvme connect` to `<CurrentIP>:<nvmePort>` for the export's NQN,
     then: filesystem mode → `mkfs` if unformatted + mount (fs-on-zvol); block
     mode → bind-mount the raw device node.
   The NVMe-oF subsystem NQN is read from the child `NetworkExport.status.NQN`
   (falling back to `spec.nvmeof.nqn`); an absent/empty NQN yields
   `FailedPrecondition` ("export not ready"), which naturally gates publish on the
   operator having rendered and the aggregator having configured the export.

4. **Privileged host operations behind a `NodeMounter` interface.** All mounts,
   `mkfs`, and `nvme connect/disconnect` go through
   [internal/csi/mount.go](../internal/csi/mount.go) `NodeMounter`, with a
   host-exec-aware command runner (`chroot`/`nsenter`, reusing the discovery
   plane's `zpool.HostExec`). The interface lets the routing logic be unit-tested
   with a fake (no real host). The node image bundles `nfs-common` + `nvme-cli` +
   `util-linux` + mkfs helpers so in-container mounting works by default;
   `--host-exec-mode` switches to the host's binaries (e.g. Talos).

5. **Publish-only (no stage/unstage).** The plugin advertises no optional node
   capabilities and does all work in `NodePublishVolume`/`NodeUnpublishVolume`.
   Publish is idempotent (an already-mounted target returns success). Unpublish
   unmounts, removes the target, and best-effort `nvme disconnect`s.

6. **Deployment.** DaemonSet (plugin + `node-driver-registrar` sidecar) with
   `hostNetwork` (to reach the storage node's NFS/NVMe endpoints), a
   `Bidirectional`-propagated `<kubeletDir>/pods` mount, the plugin/registration
   socket dirs, and `/dev`. The shared `CSIDriver` object (ADR-0001 render,
   `attachRequired: false`) covers both planes; the same driver name ties the
   registrar registration to the controller's provisioner.

### Consequences

- The node never learns an absolute path from the controller and never writes
  CRDs; its only inputs are the `volume_context` and read-only `ZfsPool` /
  `NetworkExport` status.
- A pool that has moved or died is fenced cleanly at mount time.
- `csi-sanity` node tests and live NFS + NVMe-oF pod mounts are the manual
  verification steps (not unit-tested); the fake-mounter unit tests cover routing,
  fencing, protocol dispatch, idempotency and unpublish.

---

## ADR-0002 — `poolGUID` and `datasetPrefix` are StorageClass-only

**Status:** Accepted (2026-07-17) · **Scope:** Step 6 (`cmd/csi-controller`), Helm chart

### Context

ADR-0001 defined a three-layer parameter inheritance chain (provisioner defaults
< StorageClass parameters < PVC annotations). Two of those keys select *where*
data lands: `poolGUID` picks the physical ZFS pool, and `datasetPrefix` scopes
the dataset namespace within it. If either could be set from the defaults layer
or, worse, from a PVC annotation, then:

- a cluster-wide default could silently route volumes to the wrong pool; and
- a namespace tenant authoring a PVC could redirect provisioning onto another
  pool or escape its dataset prefix — a tenancy/isolation hole.

### Decisions

1. **`poolGUID` and `datasetPrefix` are StorageClass-only.** They are honoured
   *only* from `CreateVolumeRequest.Parameters` (the StorageClass layer). If they
   appear in the provisioner-defaults layer or in the PVC-annotation layer they
   are dropped during resolution. Implemented as `storageClassOnlyParams` in
   [internal/csi/params.go](../internal/csi/params.go); other keys (`protocol`,
   `volblocksize`, `property.*`) keep the full
   inheritance chain.

2. **No default `poolGUID`.** There is no cluster-wide default pool. Every
   StorageClass must name its pool explicitly; `poolGUID` remains required, so a
   StorageClass that omits it fails `CreateVolume` with `InvalidArgument`. The
   Helm `csiController.defaultParameters` value therefore must not carry
   `poolGUID`/`datasetPrefix` (documented inline in `values.yaml`).

3. **StorageClasses are declared in the Helm chart.** `values.yaml` exposes a
   `storageClasses` list (empty by default — the chart installs none), rendered by
   `templates/storageclasses.yaml`. Each entry carries a `name` and sets
   its own `parameters` (including the required `poolGUID` and optional
   `datasetPrefix`), `reclaimPolicy`, `volumeBindingMode`, etc.

### Consequences

- Pool routing and dataset scoping are fixed by cluster administrators at
  StorageClass-authoring time and cannot be overridden by PVC authors.
- `defaultParameters` stays useful for genuinely global, safe defaults
  (`protocol`, ZFS `property.*`), not placement.
- Tests cover the restriction: `TestResolveParameters_StorageClassOnly` and the
  updated `TestCreateVolume_PVCAnnotationsOverride` assert the SC-only keys ignore
  the defaults/annotation layers while non-restricted keys still inherit.

---

## ADR-0001 — CSI controller: provisioning model, protocol/type/volumeMode axes, parameter inheritance

**Status:** Accepted (2026-07-16) · **Scope:** Step 6 (`cmd/csi-controller`)

### Context

The CSI controller is a thin, unprivileged gRPC adapter driven by
`external-provisioner`. It must turn a PVC into the ZFS-centric CRDs
(`ZfsDataset` + `ZfsShare`) and never returns an absolute path — only a
`volume_context`. Several forks needed pinning before implementation.

### Decisions

#### 1. Pool selection — fixed per StorageClass

`spec.poolGUID` is taken from a StorageClass parameter (resolvable via the
inheritance chain below); one StorageClass targets one pool. No scheduler /
free-space picking and no CSI topology awareness in this step.

- Rationale: deterministic, no placement logic, matches the GUID-keyed routing
  model already in place. Scheduling across a pool set can be layered later
  without changing the CRD contract.

#### 2. `CreateVolume` creates **both** `ZfsDataset` and `ZfsShare` (provision-time share)

> **Superseded by ADR-0010 (attach-stage zero-trust share lifecycle).** The share
> lifecycle moved to the attach stage: `CreateVolume` now writes only the
> `ZfsDataset`; the `ZfsShare` is created on demand at `ControllerPublishVolume`
> via a `ZfsShareAttachRequest` (aggregated by the operator) and torn down on
> unpublish. The provision-time-share reasoning below is retained for history.

`CreateVolume` writes the `ZfsDataset`, waits for it to reach `Ready`, writes the
`ZfsShare`, and returns `volume_context = { poolGUID, dataset, protocol }`.

- CSI does **not** require creating an export in `CreateVolume`; its only hard
  contract is "provision storage, return `volume_id` (+ optional
  `volume_context`)." Two patterns were considered:
  - **Provision-time share (chosen):** export exists for the volume's whole
    lifetime; `NodePublish` just mounts. Keeps every CRD write in the
    unprivileged, cluster-scoped controller; the node plugin stays "dumb";
    GUID-routed shares work even with no consuming pod.
  - **Publish-time share (rejected for now):** export created per consuming node
    at `NodeStage`/`NodePublish`, torn down on unstage. Tighter security but the
    node plugin needs RBAC to write `ZfsShare`/`NetworkExport`, and shares stop
    working without a pod.
- NVMe-oF host allow-listing starts permissive on the storage network and can be
  tightened later — a security refinement, not an architecture change.

#### 3. ZFS `type` and Kubernetes `volumeMode` are independent axes

- **`protocol` fixes the ZFS `type`** (hard technical constraint):
  - `nfs` ⟹ `filesystem` dataset (only a filesystem can be NFS-exported);
  - `nvmeof` ⟹ `volume`/zvol (only a block device can be NVMe-oF-exported).
- **`volumeMode` is orthogonal and resolved by the node plugin (Step 7):**
  - `nfs` → always a mounted filesystem.
  - `nvmeof` + `volumeMode=Filesystem` → node connects the zvol, `mkfs` if empty,
    mounts it → **filesystem PVC on a zvol** (e.g. databases).
  - `nvmeof` + `volumeMode=Block` → node exposes the raw connected block device.
- Only rejected combination: `volumeMode=Block` + `protocol=nfs`.
- Consequence: the controller derives `spec.type` from `protocol` alone; it does
  **not** read `volumeMode` to pick the ZFS type. This supports both "media on an
  NFS filesystem" and "database filesystem on a zvol" from the same driver.

#### 4. Parameter inheritance — three flat layers, no templating

Parameters resolve into a single `map[string]string` (later layer wins), then
parse into the CRD specs. Deliberately simpler than democratic-csi templating.

1. **Provisioner defaults** — a YAML map in a ConfigMap the controller reads
   live from the API per `CreateVolume` (`--default-parameters-configmap`,
   sourced from Helm values; originally a mounted `--default-parameters-file`,
   later switched to a live API read so edits need no restart and nothing is
   mounted).
2. **StorageClass `parameters`** — arrive in `CreateVolumeRequest.Parameters`.
3. **PVC annotations** — `external-provisioner` runs with
   `--extra-create-metadata`, which injects
   `csi.storage.k8s.io/pvc/{name,namespace}`; the controller fetches that PVC and
   overlays annotations prefixed `param.simple-zfs-csi.io/<key>`.

Resolved keys (all optional except `poolGUID` and `protocol`, which must resolve
from some layer). `poolGUID` and `datasetPrefix` are **StorageClass-only** — see
[ADR-0002](#adr-0002--poolguid-and-datasetprefix-are-storageclass-only):

| Key | Applies to | Notes |
|-----|-----------|-------|
| `poolGUID` | ZfsDataset/ZfsShare | required; **StorageClass-only**; fixed per StorageClass |
| `protocol` | both | `nfs`\|`nvmeof` → derives ZFS `type` |
| `datasetPrefix` | ZfsDataset | **StorageClass-only**; final `dataset = <prefix>/<pv-name>` |
| `volblocksize` | zvol only | |
| `nfsClients` | ZfsShare | comma list, e.g. `10.0.0.0/8:rw` |
| `nvmeofAllowedHosts` | ZfsShare | comma list of host NQNs (empty = allow-all) |
| `property.<zfsprop>` | ZfsDataset | pass-through to `spec.properties` |

> Note: `nfsClients` and `nvmeofAllowedHosts` were later removed (ADR-0010/0011);
> client allow-lists and host NQNs are now derived per attach, not supplied as
> parameters.

Capacity: `CreateVolumeRequest.capacity_range` maps to the zvol `spec.volume.size`
and to the filesystem `spec.filesystem.quota`.

#### 5. `DeleteVolume`

Deletes the `ZfsShare` and `ZfsDataset` CRDs; finalizers on the agent/operator
drive the actual teardown (`zfs destroy`, export removal). The controller does no
direct ZFS or export work.

### Consequences

- The node plugin (Step 7) only needs `ZfsPool.status` + the `volume_context`; it
  never writes CRDs and never learns an absolute path from the controller.
- `ControllerExpandVolume` and snapshots remain optional, layered later.
- The controller stays a replaceable adapter: all reconciliation lives in the
  agent (`ZfsDataset`) and operator (`ZfsShare → NetworkExport`).
