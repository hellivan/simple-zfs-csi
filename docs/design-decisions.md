# Design Decisions

An append-only log of architectural decisions (ADR-lite). Each entry records the
decision, the context, the options weighed, and the consequences. Newest first.

The complementary conventions doc is [api-conventions.md](api-conventions.md);
the build plan is [implementation-strategy.md](implementation-strategy.md);
recurring bug classes and their guards are catalogued in
[known-pitfalls.md](known-pitfalls.md).

---

## ADR-0024 — Readiness gates evaluate the object the write returned, never a re-read

**Status:** Accepted (2026-08-04) · **Scope:** `internal/controller/zfsshare_controller.go` (`Reconcile`), `internal/controller/zfsshareattachrequest_controller.go` (`reconcileVolume`) · **Related:** [known-pitfalls.md](known-pitfalls.md) #19.

### Context

Both aggregation reconcilers write a child object with `controllerutil.CreateOrUpdate`
and then decide whether the child is *live for its current spec*, by comparing
`Status.ObservedGeneration >= Generation`. Both did that comparison on a **fresh
`Get`** issued immediately after the write — through the manager's cache.

That is a read-your-own-write, and it inverts the gate. An informer that has not
yet received the update event returns the pre-update copy, whose *old*
`ObservedGeneration` matches its *old* `Generation`. The gate therefore passes at
exactly the moment it should fail: right after the spec changed. Concretely, an
allow-list change (a second node attaching, or a node detaching) could mark the
`ZfsShare` `Bound` and the attach request `Ready` while the node-local aggregator
had not yet applied the new authorization — so `ControllerPublishVolume` returns
and kubelet mounts against an export that does not yet permit it. Rejecting that
precise situation is the stated purpose of the generation gate (ADR-0010).

### Decision

Drop the re-`Get`. `CreateOrUpdate` writes the API server's response back into the
object it was handed, so after it returns that object carries the authoritative
`Generation` *and* the server's current `Status`. Both reconcilers now evaluate
the gate on it directly, and the attach-request reconciler returns that same
object to its caller instead of a re-read one.

### Consequences

- The gate can no longer pass on stale data. When the object was just updated the
  new `Generation` outruns `ObservedGeneration`, so the share reports `Exporting`
  and requeues until the node confirms — the intended behaviour.
- When `CreateOrUpdate` is a no-op the object is the copy its internal `Get`
  produced, which may be *behind* on status. That only ever delays readiness by
  one watch event, never advances it: the failure direction is safe.
- One fewer API/cache read per reconcile in both controllers.
- Not unit-testable with the controller-runtime fake client, which has no cache
  and therefore cannot be stale against itself. The invariant is carried by the
  code comments at both sites; the cache-lag cases that *are* reproducible (by
  handing a reconciler divergent cached/API readers) are covered under ADR-0023.

---

## ADR-0023 — Decisions that destroy or move something read through the API server, not the informer cache

**Status:** Accepted (2026-08-04) · **Scope:** `internal/controller/zfsdataset_controller.go`, `internal/controller/zfssnapshot_controller.go`, `internal/controller/promote.go`, `internal/controller/zfsshareattachrequest_controller.go` (new `gateReader()` on each reconciler), `cmd/operator/main.go` · **Related:** [known-pitfalls.md](known-pitfalls.md) #19 and #4.

### Context

The agent and the operator read through `mgr.GetClient()`, i.e. the manager's
informer cache — eventually consistent by construction. That is the right default
for level-triggered reconciliation, where being a watch event behind only delays
convergence. It is the wrong default for the handful of reads whose answer
authorises an **irreversible** act:

- `beforeDestroy`'s gates (`checkSnapshotDependents`, `checkOwningSnapshotLive`,
  `checkPendingCloneDependents`, plus `assertDriverSnapshot`) decide whether a
  `zfs destroy` may proceed;
- `ZfsSnapshotReconciler.reconcileDelete` decides whether the backing clone is
  gone before tearing down the raw origin snapshot;
- `reconcileVolume` deletes the `ZfsShare` — unexporting a volume a node may be
  mounted on — when it sees no attach requests, and picks the single node a zvol
  is exported to.

Every one of these fails **open** on a stale read: an object the cache has not
received yet reads as "absent", i.e. "nothing to protect". Two properties make
that likely rather than theoretical. The objects are authored by a *different
process* — the CSI controller, which uses an uncached `client.New` and so writes
land at the API server instantly — and most of the gates read a *different kind*
than the one that triggered the reconcile (a `ZfsDataset` reconcile listing
`ZfsSnapshot`s), so the two informers have no ordering relationship whatsoever.
The window is a normal delete-then-recreate flow, not an exotic race.

That `checkPendingCloneDependents` exists at all is itself an acknowledgement of
this ordering problem (D21: "the object is created by the CSI controller before
the agent runs `zfs clone`") — it was just being enforced against a data source
that cannot see the object it is looking for.

### Decision

Each affected reconciler gains an `APIReader client.Reader` field, wired
automatically in `SetupWithManager` from `mgr.GetAPIReader()` (so production
cannot forget it) and exposed through a `gateReader()` accessor. Only the reads
listed above use it; everything else keeps the cache. The operator additionally
now **fails fast when `POD_NAMESPACE` is unset**, because the namespaced cache
scoping that keeps its Secret informers legal is conditional on that variable
(pitfall #4).

The rule this generalises to: *the cache answers "what is the world like?"; the
API server answers "may I destroy this?"*.

### Alternatives considered

- **Make every read authoritative.** Rejected: it discards the informer's entire
  purpose and turns steady-state reconciliation into API-server load, to fix a
  problem that only exists on a handful of branches.
- **Give the gates a fail-closed polarity instead** (treat "absent" as
  "blocked"). Rejected: it would block every legitimate delete, since "absent" is
  also the normal answer. Note `assertKnownDatasets` already has this polarity
  naturally — an unknown clone refuses the promote — which is why it was never
  exposed.

### Consequences

- A live LIST/GET on the delete path (rare) and on the share-teardown and
  zvol-winner paths; steady-state reconciliation is untouched.
- The window narrows to a genuine race: a live read still cannot see an object
  created a microsecond later. Ordering of the actual create/delete sequence
  remains the responsibility of finalizers and the pending-dependent gate; this
  removes the *systematic* multi-second lag on top of it.
- Reproducible in tests by handing a reconciler a cached client and an
  `APIReader` backed by different fake objects — the shape used by
  `TestZfsDatasetReconcile_DeleteGateReadsThroughAPIReader` and
  `TestAttachRequest_StaleCacheDoesNotTearDownLiveShare`.
- `detachAndCleanSnapshots`, `detachSnapshotClones`, `assertKnownDatasets` and
  `assertDriverSnapshot` now take a `client.Reader` rather than a
  `client.Client`, which also documents at the type level that they only read.

---

## ADR-0022 — The CSI `volume_context` carries no routing information at all

**Status:** Accepted (2026-08-04) · **Scope:** `internal/csi/controller.go` (`CreateVolume`; the `CtxPoolGUID`/`CtxDataset`/`CtxProtocol` constants are deleted), `internal/csi/node.go` (`NodePublishVolume`) · **Refines:** ADR-0021, which kept the cached `volume_context` fields as a fallback and kept `CreateVolume` populating them. That fallback and that population are both removed here; ADR-0021 is otherwise unchanged and still records why the live `ZfsDataset` lookup is the right resolution mechanism.

### Context

ADR-0021 made `NodePublishVolume` resolve poolGUID, dataset and protocol live from
the `ZfsDataset` CR, but left the cached `volume_context` values in place as a
fallback, and left `CreateVolume` populating them so that fallback stayed
meaningful. Reviewing that immediately afterwards, the fallback turns out to be
unjustified on every axis:

- **It cannot help in the case that motivates it.** A publish also needs
  `resolvePool`, an equally live, equally *uncached* `Get`
  ([cmd/csi-node/main.go](../cmd/csi-node/main.go) builds the node client with
  `client.New`, deliberately — pitfall #4) with no fallback of its own. If the
  API server is unreachable the publish fails regardless, so the fallback never
  buys availability.
- **The one case it does cover should fail.** `ZfsDataset` missing while a PV is
  still being mounted means mounting a dataset nothing reconciles any more.
  Failing loudly is the correct answer.
- **It silently re-creates the bug it was added to fix.** A transient API error
  would mount the stale pre-rename path with no signal at all — pitfall #18,
  reintroduced through the back door.
- **There is no compatibility to preserve.** The project is pre-1.0 with no
  installed base; "keeps already-provisioned PVs working during a rollout" buys
  nothing, and every PV that exists has its `ZfsDataset` anyway.

With the fallback gone, populating `volume_context` at `CreateVolume` has no
consumer left. Keeping it would persist an immutable, never-read mirror of
mutable ZFS state into every PV — exactly the trap catalogued as pitfalls #17
and #18, left lying around for the next reader to trust.

### Decision

`NodePublishVolume` resolves poolGUID, dataset path and protocol **exclusively**
from the `ZfsDataset` named by `VolumeId`; any failure of that lookup fails the
publish (`NotFound` when the CR is gone, `Internal` otherwise). `CreateVolume`
returns **no** `volume_context`, and the `Ctx*` key constants are deleted. The
volume id is the only thing the controller hands the node; every other fact is
re-derived live, on every call, from the objects that own it.

### Alternatives considered

- **Keep the fallback but log loudly when it fires.** Rejected: it only adds a
  code path that can fire in situations where failing is the right behaviour.
- **Keep populating `volume_context` for observability** (`kubectl describe pv`
  showing the dataset). Rejected: a stale copy that *looks* authoritative is
  worse than no copy, and `kubectl get zfsdataset <volume-id>` is the live
  answer, one command away.

### Consequences

- A publish for a volume whose `ZfsDataset` was deleted now fails `NotFound`
  instead of quietly mounting a path no controller is maintaining.
- The node plugin's publish path has exactly two live reads (`ZfsDataset`,
  `ZfsPool`), both mandatory, neither cached — a simpler, more honest contract
  than "two live reads plus one snapshot that may disagree with them".
- Test fallout worth noting: most `node_test.go` publish tests never seeded a
  `ZfsDataset` and were therefore exercising the *fallback*, not the ADR-0021
  path. They now seed one via the new `dataset(...)` helper, so they cover what
  production actually does. `TestNodePublish_MissingContext` is replaced by
  `TestNodePublish_UnknownVolumeRejected`.
- PVs created before this change keep their now-ignored `volumeAttributes`; no
  migration is needed (`volume_context` is optional in the CSI spec, and an
  empty `spec.csi.volumeAttributes` is fine for Kubernetes).

---

## ADR-0021 — `NodePublishVolume` resolves poolGUID/dataset/protocol live from `ZfsDataset`, not from cached CSI `volume_context`

**Status:** Accepted (2026-08) · **Scope:** `internal/csi/node.go` (`NodePublishVolume`, new `resolveVolume` helper) · **Full record and incident writeup:** [known-pitfalls.md](known-pitfalls.md) §18.

### Context

`CreateVolume` returns a `volume_context` map (dataset path, pool GUID, protocol),
which `external-provisioner` copies once into `PersistentVolume.spec.csi.volumeAttributes`
— a field that is immutable thereafter by Kubernetes/CSI protocol, not something this
driver controls. `NodePublishVolume` read the dataset path straight out of that cached
copy. This broke down in a real incident: an operator renamed a dataset with `zfs
rename` and updated `ZfsDataset.Spec.Dataset` to match (exactly the workflow
independent-resource-naming-redesign.md was meant to make safe, since `ObjectMeta.Name`
— the stable `VolumeId` — never changes), but every subsequent mount still used the
stale pre-rename path baked into the PV, because nothing ever re-reads
`volume_context` after volume creation.

This is the same bug class as ADR-0020/pitfall #17 (a mirrored/cached copy of external
state being trusted as authoritative), just at the CSI protocol boundary: the one
genuinely authoritative source for "what dataset backs this volume right now" is the
`ZfsDataset` CR, keyed by the immutable `VolumeId`, not the one-time `volume_context`
snapshot frozen into an unmodifiable Kubernetes object.

The same reasoning extends to `poolGUID` and `protocol`: `protocol` is a pure
function of `Spec.Type` (`ParseParams` enforces the 1:1 mapping
filesystem↔nfs, volume↔nvmeof at `CreateVolume` time — see
[internal/csi/params.go](../internal/csi/params.go)), and `poolGUID` is stored
verbatim on the CR. There was never anything protocol-specific that
`volume_context` carried that isn't already, and more durably, on the
`ZfsDataset` CR reachable via the one reference CSI already guarantees is
stable (`VolumeId`).

### Decision

`NodePublishVolume` now calls `resolveVolume(ctx, volumeID)`, which does a live `Get`
of the `ZfsDataset` CR named `volumeID` and returns its `Spec.PoolGUID`, `Spec.Dataset`,
and a `protocol` derived from `Spec.Type`. The cached `volume_context` fields are used
**only** as a fallback if that lookup errors *and* the request's `volume_context` is
itself complete (e.g. the CR is genuinely missing but this is an old/already-published
PV) — otherwise the caller gets an `InvalidArgument` naming both failures. No PV field is
touched; existing PVs need no migration. csi-node's RBAC already grants `get` on
`zfsdatasets` (added alongside ADR-0019's D10 FSType tracking), so no chart change was
required. `CreateVolume` keeps populating `volume_context` as before, purely to keep that
fallback path meaningful.

### Consequences

- A dataset rename (`zfs rename` + `Spec.Dataset` edit) now takes effect on the very next
  mount/remount, with no PV delete/recreate needed — closing the gap the incident
  exposed. The same is true of any future change to how poolGUID/protocol are
  represented, since neither is trusted from the cached copy either.
- `NodePublishVolume` now issues one extra `Get` per publish call (replacing what was
  previously zero); negligible against a cached controller-runtime client (pitfall #4
  already requires the node's client to be configured this way).
- Scope check: no other `internal/csi/*.go` call site reads `vctx[CtxDataset]`,
  `vctx[CtxPoolGUID]`, or `vctx[CtxProtocol]`; `NodeExpandVolume` already resolved
  NQN/pool live and needed no change. This driver has no `NodeStageVolume`/
  `NodeUnstageVolume` to audit.

---

## ADR-0020 — The live ZFS clone graph is the source of truth for delete-path dependents

**Status:** Accepted (2026-08-03) · **Scope:** `internal/zpool` (`ZFS.ListSnapshots`, `ZFS.Clones`), `internal/controller` (`promote.go` rewritten, `zfsdataset_controller.go`, `zfssnapshot_controller.go`), `internal/csi` (`clone.go`, `snapshot.go`) · **Supersedes:** the `promoted-onto.<name>`/`restored-by.<name>` finalizer mechanism described in ADR-0019's Decision section (that ADR is otherwise unchanged and still describes the modes, the backing clone, and `zfs promote` as the core primitive) · **Full record:** [snapshot-lifecycle-redesign.md](snapshot-lifecycle-redesign.md) D17–D26 and §9.

### Context

ADR-0019 recorded the ZFS clone graph in Kubernetes — a `promoted-onto.<name>` or
`restored-by.<name>` finalizer per dataset-to-dataset dependency — and replayed it at
delete time to decide what to promote. A code review of `ef6a39b..9db3041` found four
critical defects that all reduce to that mirror drifting from reality (full reproductions
in the linked doc §9.1): a direct PVC-to-PVC clone became permanently undeletable once its
source was deleted; `DeleteSnapshot` with a live restored PVC hung in `Terminating`
forever; two restores from one snapshot produced tracking that named the wrong object; and
chained backing clones — a state this project's own live-pool run had already recorded —
were not tracked at all.

The mirror cannot be made reliable by making it more thorough. A single `zfs promote`
rewrites **four** edges at once: it relocates the origin snapshot *and every snapshot
older than it* onto the promoted clone, re-parents every sibling clone of those snapshots,
gives the promoted clone the former parent's previous origin, and turns the former parent
into a clone of it. The implementation recorded one of the four. Recording all four would
still leave the mirror one interrupted reconcile away from being wrong, because nothing
makes "perform the ZFS operation" and "update the mirror" atomic.

The defects were invisible to CI because the `fakeZFS` double had no snapshot table, a
`Destroy` that never refused, and a `Promote` that always cleared `origin` — the last of
which the project's own 2026-07-31 errata had already shown to be false.

### Decision

**Ask ZFS at the moment of deletion; delete the tracking finalizers entirely.** They are
removed, not demoted to advisory, so exactly one finalizer concept remains repo-wide:
"run the external side-effect before the object is allowed to disappear".

`beforeDestroy` now enumerates a dataset's own snapshots (`zfs list -t snapshot -d 1 -s
creation`) and each snapshot's dependents (the `clones` property — one property read, no
pool-wide enumeration), promotes dependents away in a bounded fixpoint until none remain,
then destroys whatever driver-owned artifacts are left. Nothing is remembered between
reconciles, so nothing can go stale and a crash mid-sequence simply resumes from the truth.

Kubernetes still decides whether deletion may proceed at all, but only by reading
*desired* state — `spec` and object lifecycle, never derived bookkeeping: an in-flight or
integrated-mode dependent snapshot blocks (D3/§3.2), a live owning `ZfsSnapshot` blocks
(closing the one genuine data-loss path found), and a dependent that declares this dataset
as its clone source but has not been provisioned yet blocks (D21) — that last one covering
strictly more of the restore race than the finalizer ever did.

Anything the driver did not create is refused loudly rather than guessed at (D18): a
snapshot is destroyed only if its short name matches a driver-created form, a clone is
promoted only if it maps to a known `ZfsDataset`, and a `csi-snap-<uuid>` that a live
`ZfsSnapshot` still claims is never touched. There is no fallback to `zfs destroy -r`.

**Rejected:** extending the finalizer mirror to cover the remaining three edges (the code
review's own recommendation). It grows the mechanism that produced every one of the four
defects while leaving the atomicity gap untouched, and it would additionally require
re-reading each dependent's origin *after the whole batch* rather than after its own
promote, since a later sibling promote invalidates an earlier reading.

### Consequences

- All four defects are fixed, each with a named regression test (linked doc §9.1). Their
  shared root cause is catalogued as [known-pitfalls.md](known-pitfalls.md) class 17,
  "Mirroring external (ZFS) state in Kubernetes objects".
- Correctness now rests on the test double's fidelity, so that fidelity is itself tested:
  `TestFakeZFSPromote_MatchesLivePoolVerification` replays the 2026-07-31 six-snapshot
  scrambled-order run and reproduces its converged final state verbatim, chained non-empty
  origins included.
- Promoting *one* dependent detaches a snapshot from all of them, because ZFS re-parents
  the siblings in the same operation. Sequences that previously issued N promotes now
  issue one.
- Dependency *provenance* is unaffected and needed no replacement (D20): creation lineage
  already lives immutably in `spec` (`Source.Snapshot`/`Source.Volume`, `SourceVolume`,
  owner references). Only the promote *action trail* was unrecorded, and structured log
  lines cover it. A `status.dependents` mirror and Kubernetes Events were both rejected —
  the former is derivable from `zfs list -o name,origin`, the latter expires in about an
  hour.
- Cost: one `zfs list` plus one property read per snapshot on the delete path, and the
  loss of the `kubectl`-side view of current dependencies.
- A clone or snapshot created outside the driver inside its own `datasetPrefix` now blocks
  that dataset's deletion until a human resolves it. Accepted: the prefix is designated to
  the driver and administrators only intervene there in emergencies.
- Landed in the same pass, recorded separately as D24/D25 in the linked doc:
  `ZfsSnapshot.Spec.Mode` and `Spec.SourceType` became immutable via CEL while every
  *location* field stayed deliberately mutable ([api-conventions.md](api-conventions.md)
  §5), and `CreateSnapshot` now captures the source's `fsType`/`volblocksize`/properties so
  ADR-0019's D10 compatibility checks still apply once the source volume is gone — which,
  for `standalone` snapshots, is the normal case rather than an edge one.
- **Verified against a real pool** (2026-08-03,
  [delete-path-verification-2026-08-03.md](delete-path-verification-2026-08-03.md)). The
  2026-07-31 run executed no `zfs destroy` at all, which is precisely where the four
  defects lived, so the destroy half was re-run separately: all four scenarios against the
  live `spinning-archive` pool in an isolated scratch subtree, 18 checks, 0 failures, and
  zero uses of `zfs destroy -r`. Real ZFS matched the Go test double in every case,
  including where the model contradicted a hand-written expectation.

---

## ADR-0019 — Independent snapshot lifecycle via `zfs promote`; dual standalone/integrated mode

**Status:** Accepted (2026-08-02) · **Scope:** `internal/zpool` (`ZFS.Promote`), `internal/controller` (`zfsdataset_controller.go`, `zfssnapshot_controller.go`, new `promote.go`), `internal/csi` (`clone.go`, `snapshot.go`), `api/v1alpha1` (`ZfsSnapshotSpec.Mode`), chart (`csiController.snapshotter.defaultMode`) · **Full record:** [snapshot-lifecycle-redesign.md](snapshot-lifecycle-redesign.md).

### Context

`ZfsDatasetReconciler`'s `DeleteVolume` path ran `zfs destroy -r`, which silently destroys
all of a volume's own ZFS snapshots when the volume itself is deleted — violating the CSI
requirement that a volume's snapshots survive its deletion. The full investigation (D0-D16
in the linked doc) considered Ceph-style "trash" (rename+defer) and rejected it: a ZFS
dataset with live snapshots has no operation to free only its own private data while
leaving snapshots attached, so trash would leave the entire deleted volume's storage
allocated for as long as any snapshot survives — an unbounded, silent leak on this
project's own long-retained-backup-snapshot use case. `zfs promote` (reversing a clone's
parent/child relationship with its origin snapshot) is the ZFS-native primitive Ceph's RBD
lacks, and reclaims the deleted volume's own data immediately and unconditionally instead.

### Decision

Two selectable `ZfsSnapshotSpec.Mode` values, resolved at `CreateSnapshot` time from a
`VolumeSnapshotClass` `mode` parameter (falling back to `csiController.snapshotter.defaultMode`,
chart default `standalone`; unset/legacy snapshots are treated as `integrated`):

- **`standalone`** (new default): `CreateSnapshot` takes the raw snapshot, then provisions
  an owned, child `ZfsDataset` "backing clone" from it (flat sibling under the source's own
  prefix, named after the already-independent `Spec.SnapshotName` — no new naming scheme
  needed thanks to ADR-0018), and takes a fixed `@restore-source` self-snapshot on it.
  Restores always clone from `<backing-clone>@restore-source`, never the raw snapshot.
  `DeleteVolume` promotes every `Ready` standalone dependent's backing clone away
  (unconditionally) instead of blocking, then destroys the source with a **non-recursive**
  `zfs destroy` (safe now that every tracked dependent has been promoted off it first).
  `DeleteSnapshot` deletes the backing-clone `ZfsDataset` object and waits for
  `ZfsDatasetReconciler`'s own promote-then-destroy delete path to finish it, then performs
  the required (not best-effort) raw-origin-snapshot cleanup on the true source.
- **`integrated`** (today's original behavior): plain `zfs snapshot`, no backing clone.
  `DeleteVolume` blocks (requeues) while any live `integrated`-mode dependent exists —
  there's nothing to promote in this mode.

Direct PVC-to-PVC clones (ADR-0009, no `VolumeSnapshot` involved) are always promoted away
unconditionally on `DeleteVolume`, no mode toggle. A generalized `promoted-onto.<name>`/
`restored-by.<name>` finalizer mechanism tracks any dataset-to-dataset dependency the
promote machinery creates (e.g. two simultaneous restores from one snapshot — promoting
one reparents the other onto it instead of freeing it, per real, documented ZFS behavior),
so every object stays independently deletable in any order. Cross-`datasetPrefix` restores
are rejected outright (`InvalidArgument`) since a backing clone's origin must stay inside
the same replicated subtree for `zfs send -R` backup compatibility.

### Consequences

- Snapshots now genuinely survive their source volume's deletion in `standalone` mode
  (the actual bug motivating this work), with no CO-visible blocking.
- **D10 (property/fsType compatibility on clone/restore) and `Status.FSType` tracking were
  initially deferred, then implemented in a follow-up pass (2026-08-03)**: `ZfsDataset.Status.FSType`
  is set once by the node plugin the first time it formats a zvol (`internal/csi/node.go`'s
  `recordFSType`, best-effort). `internal/csi/mount.go`'s `FormatAndMount` skips a redundant
  `mkfs` when the device is already formatted with the *matching* requested type, and **fails
  loudly** (refuses to mount) if the device already carries a *different* type — verified
  against actual upstream practice (`k8s.io/mount-utils`'s `formatAndMountSensitive` and
  ceph-csi's `mountVolumeToStagePath`, both of which still attempt the mount with the
  requested type and let it fail rather than silently substituting the on-disk one); an
  earlier version of this fix incorrectly auto-substituted the existing type; corrected to
  match both upstream behavior and this project's own ADR-0013 "fail loud on
  misconfiguration" precedent. `resolveContentSource`
  now rejects (`InvalidArgument`) a clone/restore whose resolved `volblocksize`, `property.*`
  overrides, or requested `fsType` diverge from the source's recorded/actual values, for both
  content-source paths and both snapshot modes; absent source data (deleted source, or a
  never-formatted volume) is treated as unconstrained, not as a mismatch.
- Test coverage uses a fake `ZFS` double (including a `Promote` model of ZFS's real
  sibling-clone reparenting); the D16 promote-ordering fixpoint loop's real-pool edge cases
  are not independently re-verified here, relying instead on the linked doc's own
  2026-07-31 live-pool verification.

---

## ADR-0018 — Independent, opaque ZFS-facing names for `ZfsDataset`/`ZfsSnapshot`

**Status:** Accepted (2026-08-02) · **Scope:** `internal/csi` (`CreateVolume`, `CreateSnapshot`, `ensureVolume`, `ensureSnapshot`, `volumeSpecCompatible`), `internal/controller/zfsdataset_controller.go` (`clone()`) · **Full record:** [independent-resource-naming-redesign.md](independent-resource-naming-redesign.md).

### Context

`CreateVolume`/`CreateSnapshot` fed the CO-provided CSI `Name` directly into both
the Kubernetes object identity (`ObjectMeta.Name`) and the literal ZFS-facing
path (`ZfsDataset.Spec.Dataset`, `ZfsSnapshot.Spec.SnapshotName`). Investigation
(detailed in the linked doc) confirmed this was always safe in practice —
`external-provisioner`/`external-snapshotter` only ever pass Kubernetes-generated
UUIDs, never raw user text — but coupling the ZFS path to an upstream sidecar's
internal naming convention (an implementation detail, not a CSI-spec guarantee)
was still worth removing, Ceph-CSI-style, at a fraction of the cost of a full
volume/snapshot journal (which would have required decoupling `ObjectMeta.Name`
itself, touching ~80 call sites).

### Decision

Keep `ObjectMeta.Name` exactly as the CSI-provided `VolumeId`/`SnapshotId`
(zero changes to any of the ~80 existing call sites keyed off it, and idempotency
stays free via etcd name uniqueness). Generate a fresh, independent, opaque leaf
identifier for the two ZFS-facing fields on first creation only, using the
already-vendored `github.com/google/uuid`:
- `ZfsDataset.Spec.Dataset`'s leaf: `csi-vol-` + `uuid.New().String()`.
- `ZfsSnapshot.Spec.SnapshotName`: `csi-snap-` + `uuid.New().String()`.

On an idempotent retry (`ensureVolume`/`ensureSnapshot` finds an existing
object), the candidate is discarded and the already-persisted value is treated
as authoritative — both helpers now return the persisted spec instead of `nil`
so the caller can pick up the real dataset/snapshot-name. `volumeSpecCompatible`
and `ensureSnapshot`'s compatibility check were updated to drop the
`Dataset`/`SnapshotName` equality requirement accordingly (no longer
deterministically recomputable by the caller).

ADR-0009's direct-clone path (`zfsdataset_controller.go`'s `clone()`) had the
same exposure in its intermediate `"@clone-" + vol.Name` snapshot suffix; this
is purely internal/ephemeral (re-derived fresh every reconcile, never
persisted), so it was closed more cheaply by hashing the destination object
name (`cloneSnapshotSuffix`, SHA-256 truncated to 16 hex chars) rather than
adding a new persisted field.

### Consequences

- `resolveContentSource`, `ListSnapshots`, status reporting, etc. needed no
  changes — they already read `Spec.Dataset`/`Spec.SnapshotName` off the
  fetched object rather than recomputing a path from the CSI id.
- The snapshot-lifecycle redesign's backing-clone naming can now reuse
  `Spec.SnapshotName` directly (already independent/opaque) instead of the raw
  CSI snapshot name.

---

## ADR-0017 — Keep one CSI driver for both protocols; reject cross-protocol/type restores in-controller

**Status:** Accepted (2026-07-29) · **Scope:** `internal/csi` (`resolveContentSource`, `ZfsSnapshotSpec.SourceType`) · **Builds on** the single-driver StorageClass/protocol model (ADR-0002), the `ZfsDataset`/`ZfsSnapshot` CRD taxonomy (ADR-0006, ADR-0008) and the same-pool/same-type clone/restore checks (ADR-0009).

### Context

A single CSI driver name (`simple-zfs-csi.io`) serves both protocols (`nfs` =
filesystem, `nvmeof` = zvol), selected purely by the StorageClass `protocol`
parameter (ADR-0002). This raised the question of whether a snapshot of one
dataset type could be restored into a volume of the other type — e.g. a
filesystem PVC's snapshot restored via an `nvmeof` StorageClass — and whether
that class of mismatch is actually guarded against, given Kubernetes itself
provides no help here (unlike, say, ceph-csi, where `rbd.csi.ceph.com` and
`cephfs.csi.ceph.com` are separate driver names, so `external-provisioner`
refuses to route a cross-backend restore before it ever reaches the driver).

Investigation confirmed the mismatch is already rejected in
`resolveContentSource` (ADR-0009): it compares the source's dataset type
against the target StorageClass's protocol-derived type and returns
`InvalidArgument` on a mismatch. The one gap found was that the source type was
previously derived only via a live lookup of the source `ZfsDataset`, which
returns "unknown" (skipping the check) once that source object is deleted —
e.g. the original PVC was removed but its snapshot retained. This was closed by
recording `SourceType` on `ZfsSnapshotSpec` itself, captured immutably at
`CreateSnapshot` time, mirroring the precedent set by Kubernetes'
`VolumeSnapshotContent.spec.sourceVolumeMode` (also immutable, also captured at
snapshot creation for the same reason).

This left the broader question: given ceph-csi's precedent of splitting by
backend, would splitting this driver into two (e.g.
`nfs.simple-zfs-csi.io` / `nvmeof.simple-zfs-csi.io`) be the better design here
too?

### Decision

**Keep the single CSI driver name for both protocols.** The in-controller
type/protocol check (now hardened against source deletion) is sufficient, and
the one-driver model already matches how CSI drivers conventionally serve
multiple volume modes from a single driver (e.g. `Filesystem` vs `Block`
`VolumeCapability.AccessType`, as EBS/GCE PD/ceph-csi's own RBD driver do) —
the `protocol` StorageClass parameter plays that same role here.

### Alternatives considered

- **Split into two CSI driver names, one per protocol.** Would make the
  mismatch structurally impossible at the Kubernetes plumbing layer (wrong
  driver name never routes to the controller at all), matching ceph-csi's
  `rbd.csi.ceph.com`/`cephfs.csi.ceph.com` split. Rejected: ceph-csi splits by
  *backend* (RBD vs CephFS are different storage systems with independent
  provisioning APIs and sidecar/scaling needs); this project's `nfs` vs
  `nvmeof` split is already made at that layer — `nfs-controller` and
  `nvmeof-controller` are separate binaries/Dockerfiles/DaemonSets. Only the
  CSI *control plane* (provisioning/attach/snapshot bookkeeping against the
  same ZFS pools) is shared, which is the normal scope for one driver name.
  Splitting would double the `CSIDriver` object, the controller Deployment and
  all four of its sidecars (provisioner/resizer/attacher/snapshotter), the node
  plugin's kubelet registration, and the RBAC surface — a large, ongoing
  maintenance cost to trade an already-tested ~15-line in-controller check for
  a Kubernetes-level one.
- **Do nothing (rely only on the live `ZfsDataset` lookup).** Rejected: leaves
  the type check silently skipped once the source dataset is deleted, the
  scenario this ADR set out to close.

### Consequences

- No chart/API-visible change beyond `ZfsSnapshotSpec.SourceType`
  (`internal/csi/snapshot.go`, `internal/csi/clone.go`); existing clusters'
  pre-upgrade `ZfsSnapshot` objects have `SourceType: ""` and transparently fall
  back to the old live-lookup behavior (no migration needed, but they remain
  exposed to the source-deleted edge case until recreated).
- The in-controller checks remain the single enforcement point for
  cross-protocol/type restores; the driver-name split is not planned unless
  NFS and NVMe-oF need independent scaling, RBAC, or upgrade lifecycles in the
  future, at which point this ADR should be superseded.

---

## ADR-0016 — Default `hostExec.mode` to `nsenter`; `chroot` cannot create host mounts on Talos

**Status:** Accepted (2026-07-24) · **Scope:** Helm `values.yaml` defaults for `discovery.hostExec.mode`, `csiNode.hostExec.mode` and `toolbox.hostExec.mode` (`chroot` → `nsenter`), and the operational guidance in [known-pitfalls.md](known-pitfalls.md) class 15 · **Builds on** the `Runner` host-exec indirection (ADR-0003) and the host-exec observability/mode work (ADR-0013).

### Context

Host-exec runs the host's `zpool`/`zfs`/`mount`/`mkfs` from inside a pod, in one of
two modes:

- **`chroot`** bind-mounts the host root at `/host` and runs `chroot /host <tool>`.
  This changes *path resolution* but **not the mount namespace** — the process stays
  in the pod's namespace.
- **`nsenter`** enters the target PID's (PID 1) **mount namespace** via `hostPID` +
  `/proc/1/ns/mnt`, so the tool runs as if on the host.

For read-only commands (`zpool status`, `zfs list`, `zpool scrub`) the two are
equivalent. The difference bites for commands that **create a mount**: discovery's
`zfs create`/`zfs clone` auto-mount the new dataset; an enabled csi-node runs
`mount`/`mkfs`.

Field finding (Talos, single storage node, pool at `/var/mnt/spinning-archive`):
with `discovery.hostExec.mode: chroot`, **every NFS (filesystem) PVC dataset was
left `mounted no` on the host**. `zfs create` auto-mounted each dataset inside the
discovery pod's namespace under `/host`, that mount never propagated to the host, and
it vanished when the pod restarted. Reads/writes to the "mounted" path (via the NFS
export and via the toolbox `/host` path) therefore landed in the **parent** dataset —
data silently written to the wrong dataset, child `USED` staying at 96K. zvols
(NVMe-oF) were unaffected (block export, no filesystem mount).

Why chroot cannot be fixed on Talos: escaping the pod requires the `/host` volume to
be `Bidirectional` (rshared) **and** the host source to be a shared mount. The
discovery `/host` source is `hostPath: /`. The Talos kubelet only propagates paths
declared in `machine.kubelet.extraMounts` (here only `/var/mnt/spinning-archive`,
`rshared`); `/` is not — and rsharing the entire root into the kubelet is
impractical. So `Bidirectional` either fails pod startup (non-shared source) or, as
observed, creates the mount but leaves it trapped.

### Decision

Default `hostExec.mode` to **`nsenter`** for every component that uses host-exec
(`discovery`, `csiNode`, `toolbox`; the scrub CronJob follows `discovery.hostExec`).
`nsenter` creates the mount **directly in the host's mount namespace**, under the
pool path Talos already shares via `extraMounts`, so it propagates to the NFS server
pod and consumers with no `/host` volume, no Bidirectional plumbing, and no
whole-root rshare. `chroot` remains selectable but is documented as **read-only-safe
only**, and as **incorrect on Talos for any mount-creating operation**.

### Alternatives considered

- **Keep `chroot` default + `Bidirectional` `/host` (the prior guard).** Rejected: it
  only works where host `/` is rshared, which is not the case on Talos and cannot be
  arranged cleanly; it fails loudly (pod won't start) or, worse, silently traps the
  mount. It also mounts the entire host root Bidirectional — heavy and risky.
- **Per-pool `hostPath` at the pool mountpoint with in-container Bidirectional.**
  Rejected for the generic agent: discovery/csi-node don't know the pool path a
  priori, and it would still require bundling matching ZFS userspace in-image.
- **Bundle ZFS userspace in the image and mount in-container (drop host-exec).**
  Rejected per the ADR-0003 rationale: in-image tools drift from the host ZFS kernel
  module version; host-exec exists precisely to avoid that drift.
- **`nsenter` only for discovery, keep `chroot` for the toolbox (`/host` browse).**
  Partially valid — chroot is fine for the toolbox's read-only use — but a `zfs mount`
  run from a chroot toolbox silently fails to affect the host, a footgun. We default
  the toolbox to `nsenter` for correctness and document that browsing moves to
  `toolbox.datasetMountRoot` or `nsenter -t 1 -m -- ls`.

### Consequences

- Dynamically provisioned NFS datasets mount on the host and export correctly on
  Talos out of the box; the silent "data lands in the parent" class is closed for new
  provisioning.
- All host-exec pods now request `hostPID: true` (already required for nsenter) — an
  acceptable posture for privileged, node-local storage agents.
- The toolbox no longer bind-mounts `/host` by default (that volume is chroot-only).
  Browse dataset mountpoints via `toolbox.datasetMountRoot` (may be a parent of the
  pool mountpoint) or `nsenter -t 1 -m -- ls`.
- **Not retroactive:** the reconciler is idempotent-create, so switching an existing
  cluster to `nsenter` fixes *future* provisioning only. Datasets already created
  under `chroot` remain unmounted until explicitly `zfs mount`-ed in the host
  namespace, and any data written into a parent while a child was unmounted is
  shadowed once the child mounts and must be reconciled by hand.
- `chroot` stays available for non-Talos hosts / read-only use; the chart values and
  known-pitfalls class 15 carry the warning.

---

## ADR-0015 — Provision-time POSIX root ownership for filesystem datasets via `uid`/`gid`/`mode` parameters

**Status:** Accepted (2026-07-27) · **Scope:** `internal/csi/params.go` (new `uid`/`gid`/`mode` params), `internal/csi/controller.go` (`volumeSpec`), `api/v1alpha1/zfsdataset_types.go` (`FilesystemConfig.UID/GID/Mode`), `internal/zpool/zfs.go` (`ZFS.ApplyOwnership`), `internal/controller/zfsdataset_controller.go` (apply-once at create) · **Builds on** the three-layer parameter inheritance (ADR-0002) and complements the Helm `fsGroupPolicy`/`--default-fstype` fix (see [known-pitfalls.md](known-pitfalls.md) class 14).

### Context

Kubernetes applies a pod's `securityContext.fsGroup` by recursively `chown`-ing the
volume at mount time — but **only for single-node (RWO) block volumes** whose PV
carries an `fsType`. For an NFS (RWX) volume kubelet deliberately skips fsGroup: a
recursive chown of a shared export is expensive and semantically wrong (many pods,
possibly many nodes, share the tree). So a freshly provisioned NFS dataset is always
`root:root 0755`, and a non-root workload cannot write to it without a manual
`chown` on the server. There was no way to say "this share should be owned by uid
1000" at provision time.

fsGroup solves the block case; it cannot solve the share case. The share case has to
be handled **server-side, once, at creation**.

### Decision

Introduce three optional CSI parameters — `uid`, `gid`, `mode` — that set the POSIX
ownership and permission bits of a **filesystem (nfs) dataset's root**, applied
exactly once immediately after the dataset is created:

1. **Filesystem-only.** The params are parsed only when the resolved dataset type is
   `filesystem`. For `nvmeof` (block) they are **silently ignored**, not rejected, so
   a cluster-wide default (set in the provisioner `defaultParameters` layer) does not
   break block provisioning. Block ownership is a fsGroup concern, not ours.

2. **Inherited + PVC-overridable.** They flow through the existing three-layer
   inheritance (`defaults → StorageClass → PVC annotations`, ADR-0002), so an admin
   can set a cluster or StorageClass default and a PVC can override per-claim via the
   `param.simple-zfs-csi.io/{uid,gid,mode}` annotations. They are ordinary params,
   **not** StorageClass-only like `poolGUID`/`datasetPrefix`.

3. **Opt-in / backward compatible.** Unset means *do nothing* — no chown, no chmod —
   leaving the ZFS default (`root:root`, `0755`). Existing installs are unaffected.

4. **Apply-once at creation, never re-applied.** The reconciler calls
   `ZFS.ApplyOwnership` on the dataset mountpoint only in the create-absent branch,
   right after `create()` succeeds. It is **not** reconciled on every pass: the values
   are an initial seed, not an enforced invariant, so that a workload (or an admin)
   remaining free to re-chown files inside the share is never fought by the operator.
   A failure sets the dataset status to `OwnershipFailed` and requeues.

5. **Executed on the host.** `CLI.ApplyOwnership` runs `chown`/`chmod` through the
   same host runner (`HostExec.BuildRunner`) already used for `zfs`, so the ownership
   change lands on the real mounted filesystem in the host mount namespace, not the
   container's.

Validation: `uid`/`gid` must parse as non-negative integers; `mode` must be a valid
octal string (e.g. `0770`). Malformed values fail `CreateVolume` fast rather than
silently doing the wrong thing.

### Alternatives considered

- **Rely on fsGroup for NFS too.** Rejected: kubelet does not apply fsGroup to RWX
  volumes, and forcing it (via `--default-fstype` + `File` fsGroupPolicy on the
  shared export) would trigger an expensive, semantically wrong recursive chown that
  also fails under NFS `root_squash`. See known-pitfalls class 14.
- **Reconcile ownership on every pass (enforce as invariant).** Rejected: it would
  clobber legitimate in-share ownership changes made by the workload and add a
  recursive-chown cost on a shared tree on every reconcile. Seed-once matches the
  fsGroup mental model (set at provision, not policed).
- **A single `owner` string (`uid:gid:mode`).** Rejected in favour of three discrete
  params: they map cleanly onto the inheritance layers (a PVC can override just
  `gid`), onto `chown` vs `chmod`, and onto individual validation errors.
- **Name it `permissions` instead of `mode`.** Rejected: `mode` matches the POSIX /
  `chmod` / Kubernetes `defaultMode` vocabulary and is unambiguously the octal bits.

### Consequences

- NFS shares can be provisioned owned by an arbitrary uid/gid/mode without any manual
  server-side step, closing the gap fsGroup leaves for RWX volumes.
- The knobs are inert for block volumes and inert when unset, so the change is safe to
  ship on by default in the chart's commented defaults.
- Because ownership is seeded once, an admin who later changes the StorageClass
  `uid`/`gid`/`mode` will **not** see existing datasets re-chowned; only new datasets
  pick up the new default. This is intentional (see decision 4) and documented in the
  chart values.

---

## ADR-0014 — NVMe-oF DH-CHAP secret handling: uncached namespaced reads, immutable per-attach keys, rotate-by-reattach

**Status:** Accepted (2026-07-20) · **Scope:** `internal/controller/nvmeof_controller.go` secret read path (new `SecretReader` on `NVMeoFReconciler`, wired to `mgr.GetAPIReader()` in `cmd/nvmeof-controller`), and the operational contract for DH-CHAP key rotation · **Builds on** the NVMe-oF zero-trust per-attach DH-CHAP design (ADR-0011) and the incremental nvmet configfs reconcile (ADR-0009).

### Context

On the live cluster all three NVMe-oF volumes were stuck: `ZfsShare` never left
`Exporting`, the `NetworkExport` had no phase, the `ZfsShareAttachRequest` never went
Ready, and pods hung in `ContainerCreating` with `FailedAttachVolume … DeadlineExceeded`.
The NFS volume on the same pod attached fine. The nvmeof controller was spamming:

```
secrets is forbidden: User "system:serviceaccount:zfs-shares:…-nvmeof"
cannot list resource "secrets" in API group "" at the cluster scope
```

The reconciler read the DH-CHAP secret with the manager's **cached** client
(`r.Get(secret)`). Per ADR-0011 the secrets RBAC is intentionally a **namespaced
`Role`** (`-dhchap-reader`, least privilege). But a cached read is not a targeted
GET: the first Secret read through the cached client lazily starts a Secret
**informer**, which does a **cluster-wide LIST+WATCH** to populate its cache — and
that cluster-scoped list is exactly what the namespaced Role forbids. The informer
never synced, `dhchapKey()` never returned, the nvmet target was never programmed,
and the whole NVMe-oF attach chain stalled. csi-node was unaffected because its CSI
server uses a **direct (uncached)** client.

Investigating the fix raised two follow-on questions that turned into durable
contract: **(a)** can a DH-CHAP key be rotated while a volume is attached, and
**(b)** what happens to a mounted volume / its pod when the NVMe-oF connection dies.

### Decisions

1. **Read DH-CHAP secrets uncached, via `mgr.GetAPIReader()`.** `NVMeoFReconciler`
   gained a `SecretReader client.Reader` field set to the manager's API reader; only
   the `dhchapKey()` read uses it (falls back to the cached client when nil, for
   tests). This issues a **direct namespaced GET** to the API server — no informer,
   no cluster-wide LIST — so the existing namespaced `Role` is sufficient and no
   Secret type is ever cached. This matches how csi-node already reads the secret.
   RBAC was **not** broadened to a cluster-scoped ClusterRole: that would violate
   ADR-0011's least-privilege posture (a node-local component able to list every
   Secret in the cluster) to work around a client-caching detail.

2. **Do not watch the Secret / do not rotate the target key under a live client.**
   A Secret watch was rejected even scoped to the namespace. The nvmet configfs
   reconcile *is* incremental and would let us rewrite `hosts/<h>/dhchap_key` without
   disturbing an established association — but the Linux **host reconnects with the
   key it was given at connect time** (stored in the controller options; csi-node is
   not consulted on a kernel-level reconnect). So rotating the target key under a
   connected client is a latent footgun: the connection works until the first
   transport blip, then the kernel auto-reconnect presents the **old** key against
   the **new** target key → auth fails → controller lost → device dropped. A normally
   invisible blip becomes a permanent volume failure.

3. **DH-CHAP keys are per-attach and immutable; rotate by re-attach.** The operator
   mints one `dhchap-pvc-…` Secret per attachment and deletes it at detach; the key
   never changes during an attachment's lifetime. The correct rotation path is to
   **tear down and re-establish the attachment** (fresh connect re-reads the current
   Secret on both target and host), not to edit the Secret in place. Because the
   secret is immutable, an uncached read loses nothing (there is no update to react
   to) and a watch would fire ~never; drift after a node reboot is already handled by
   the controller rebuilding nvmet from the **watched** `NetworkExport`s on startup.

### Consequences

- **NVMe-oF attaches work under the namespaced Role.** No RBAC change, no cluster-wide
  Secret exposure; ADR-0011 least-privilege preserved. Requires rebuilding/redeploying
  the nvmeof image for the fix to take effect on a cluster.
- **Editing a `dhchap-pvc-…` Secret is unsupported and unsafe.** It does not
  re-key a live connection, and it will break the kernel's transparent reconnect for
  any client still connected with the old key. Rotate by re-attaching the volume.
- **Connection-death behavior (operational expectation).** On an NVMe-oF/TCP transport
  drop the host retries every `reconnect_delay` (~10 s) up to `ctrl_loss_tmo` (~10 min,
  kernel defaults, tunable). Within that window I/O **blocks** and recovery is
  transparent (reconnect re-auths with the connect-time key). If `ctrl_loss_tmo`
  expires the controller is removed, the block device disappears, and I/O fails with
  **EIO** → the filesystem goes read-only/errors. **kubelet does not health-check
  volume I/O**, so the pod is *not* auto-rescheduled — it keeps running on a broken
  mount until it is deleted/rescheduled (CSI then `NodeUnstage`→`NodeStage` and issues
  a fresh `nvme connect`) or a volume-touching **liveness probe** forces a restart. A
  container restart alone does not remount the volume. Recommendation: give NVMe-oF
  workloads a liveness probe that exercises the volume.
- **Future rotation, if ever needed,** would require a coordinated drain: quiesce/detach
  the client first, then reprogram the target, then reconnect — i.e. still re-attach,
  never an in-place edit under a live client.
- **Secret cleanup is finalizer-guaranteed, not best-effort.** `deleteDHChapSecret`
  runs in the last-detach reconcile, and the attach request's finalizer is released
  only after it succeeds — so a controller crash or transient error retries rather
  than orphaning the `dhchap-pvc-…` Secret. Residual leaks require manually
  force-deleting the attach request / stripping its finalizer, or a persistent
  `secrets delete` RBAC denial (which surfaces as a stuck `Terminating` object, not
  a silent leak).

---

## ADR-0013 — Host-command observability & strict dataset creation (parents via `ZfsDataset`, no `-p`)

**Status:** Accepted (2026-07-20) · **Scope:** new `internal/zpool.LoggingRunner` (wired into `cmd/zpool-discovery` + `cmd/csi-node`), Helm global `logLevel` on all six components, and the create-time behavior of the `ZfsDataset` agent · **Builds on** the `Runner` host-exec indirection (ADR-0003) and the `ZfsDataset` allocation CRD (ADR-0006).

### Context

A provisioning failure surfaced only as `dataset create failed: … parent does not
exist`, with no record of the exact command the agent ran against the host. Because
every `zfs`/`zpool` call is rewritten by `HostExec` (chroot/nsenter + the
version-matched host binary), the *effective* command line is invisible, and there
was no switch to turn on verbose tracing — so the failing invocation could not be
inspected.

Two questions fell out of the investigation: **(a)** how to make host commands
observable on demand, and **(b)** whether `zfs create` should auto-create missing
parent datasets with `-p` — the missing parent being the actual root cause.

### Decisions

1. **One command-logging wrapper at the `Runner` choke point.** `LoggingRunner(base
   Runner, log)` wraps the single `Runner` seam every ZFS/pool/mount/nvme call flows
   through and logs each invocation plus its outcome (duration, trimmed output or
   error) at **V(1) debug**. It is passed as the *base* to `HostExec.BuildRunner`, so
   it logs the **fully resolved** host command including the chroot/nsenter prefix
   and resolved binary path (e.g. `chroot /host /usr/sbin/zfs create tank/k8s/pvc-…`).
   Wired into the two long-running host-exec agents: `cmd/zpool-discovery` (runs
   `zfs create`) and `cmd/csi-node` (mount/nvme). Kept at debug, **not** error,
   on purpose: several ZFS calls fail by design (the `zfs get` existence probe
   returns `ErrNotExist` before a create), so logging every failure at error level
   would be misleading noise. The `Runner` indirection — originally introduced for
   testability — now doubles as the observability seam, so no per-call-site logging
   is needed.

2. **Debug logging is opt-in via a Helm `logLevel` value.** A global `logLevel`
   (default empty = info) renders `--zap-log-level=<level>` on all six components;
   the flag was already bound by controller-runtime's zap options but never surfaced
   in the chart. Each component takes a `<component>.logLevel` override that falls
   back to the global (so you can raise just the discovery agent, or lower one noisy
   component under a global debug). Off by default keeps logs quiet; `--set
   logLevel=debug` turns on full command tracing when hunting a problem.

3. **`zfs create` stays strict — no `-p`.** The agent creates a dataset only when its
   parent already exists. `-p` was rejected: it is implicit, and a typo in a
   StorageClass `datasetPrefix` would silently materialize a whole stray dataset tree
   (with inherited defaults) instead of failing. Strict create makes misconfiguration
   **fail loudly**, which is the safer default for a storage provisioner.

4. **Parents are declared, not implied — via `ZfsDataset`.** The prefix/namespace
   dataset is declared as its own `ZfsDataset` object, with explicit properties
   (quota, compression, mountpoint) rather than the defaults `-p` would inherit. This
   fits the existing declarative model (you already declare the pool and label the
   node). Ordering needs no orchestration: a child PVC dataset reconciled before its
   parent simply errors and requeues until the parent `ZfsDataset` exists (eventual
   consistency). Multi-level prefixes declare each level; the pool root always exists,
   so the common single-level prefix is trivial.

### Consequences

- Every host command is traceable on demand, in its fully-resolved chroot/nsenter
  form, without redeploying different images — and silent by default.
- Provisioning is fail-loud on a missing or mistyped parent; the parent dataset
  becomes a declarative prerequisite managed through the same CRD flow, with
  first-class properties.
- Trade-off accepted: admins must pre-declare each prefix dataset; the driver will
  never conjure parents. Having the operator auto-create a prefix `ZfsDataset` from a
  StorageClass `datasetPrefix` is a possible future convenience, deliberately
  deferred to keep creation explicit.

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

   **Defended, not merely assumed (added later).** The "exactly one attach
   request" invariant can be transiently violated by a forced pod move (the
   attach-detach controller may attach node B before node A detaches). Two guards
   keep a zvol single-node: `ControllerPublishVolume` returns `FailedPrecondition`
   if the volume is already published to another node (`attachedNode`), and the
   operator aggregator exports a zvol to exactly one node — the oldest attach
   request wins, and a losing request stays not-ready (`oldestAttachNode`). See
   known-pitfalls.md class 13.

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
