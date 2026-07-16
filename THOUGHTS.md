# Talos Storage Architecture: Bare-Metal ZFS & Multi-Node Sharing
### The Problem
 * **Hardware Setup:** Moving from an Arch/KVM/TrueNAS hypervisor to a bare-metal Talos cluster.
 * **Node A (Main Server):** Has an LSI HBA passed through with spinning HDDs and an SSD SLOG running a ZFS pool.
 * **Node B (Intel NUC):** Has minimal/no storage.
 * **The Goal:** Provide Highly Available ReadWriteOnce (RWO) for PostgreSQL databases and ReadWriteMany (RWX) for shared files (Samba) across the entire cluster, using pure GitOps (no UI), without locking pods to Node A via nodeAffinity.
 * **The Challenge:** ZFS is a local, Copy-on-Write filesystem, not a clustered network filesystem. Standard local ZFS provisioners permanently lock volumes to the physical host, stranding the NUC.
### The Clustered ZFS Solution (Highly Recommended)
#### Option 1: Linstor / Piraeus Operator
The ultimate GitOps-native, ZFS-backed, hyperconverged storage engine.
 * **How it works:** Uses native ZFS commands to manage disks, but uses **DRBD** (a kernel-level network RAID) to replicate blocks across the network.
 * **How it solves Node Affinity (RWO):** Uses "Diskless DRBD Clients". If a pod spins up on the NUC, Linstor creates a virtual block device on the NUC that pipes the data over the network to the physical ZFS zvol on Node A.
 * **How it solves RWX:** Automatically spins up and manages hidden, internal NFS server pods whenever an RWX volume is requested.
 * **Talos Requirement:** You must build a custom Talos OS image containing the siderolabs/zfs and siderolabs/drbd extensions.
### The "DIY NAS" Layered Solutions (Keep ZFS, Split Provisioners)
If you use any of these options, you get zero network replication out of the box, meaning your database pods (RWO) *must* be pinned to Node A via node selectors. To get RWX, you must manually layer an internal NFS server pod (like nfs-ganesha) on top of them.
#### Option 2: OpenEBS LocalPV + NFS Ganesha
 * **How it works:** OpenEBS ZFS LocalPV handles the raw disk management and provisions local RWO volumes directly on Node A. You then deploy an NFS Ganesha Helm chart, give it a massive chunk of OpenEBS storage, and let it act as the dynamic RWX provisioner for the NUC.
 * **Pros/Cons:** 100% GitOps and rock-solid, but lacks the automatic pod-mobility and high availability of Linstor.
#### Option 3: Democratic-CSI (Local Mode) + NFS Ganesha
 * **How it works:** Identical architecture to Option 2, but uses Democratic-CSI's zfs-local-dataset driver as a DaemonSet to issue raw zfs create/destroy commands on the host instead of OpenEBS.
 * **Pros/Cons:** Good if you already prefer Democratic-CSI's configuration syntax, but OpenEBS is generally considered the more robust community standard for local Kubernetes ZFS.
#### Option 4: kubernetes-zfs-provisioner (The Niche Option)
 * **How it works:** A single external provisioner that creates ZFS datasets and automatically shares them via an NFS server running inside its own pod.
 * **The Catch:** It requires SSH access to the underlying node to execute ZFS commands. Because Talos Linux completely removes the SSH daemon for security and immutability, this is incredibly difficult to run without building heavy custom API-to-SSH proxies.
### The Non-ZFS Alternatives
#### Option 5: Rook / Ceph
The industry standard for Kubernetes hyperconverged storage.
 * **How it works:** Ceph takes raw control of the disks and provides its own native RWO (RBD) and RWX (CephFS) distributed network storage.
 * **Pros/Cons:** Seamless, native Kubernetes integration with zero NFS hacks or custom Talos kernel modules required. However, you must completely abandon ZFS, format your drives, and migrate all your data manually.
#### Option 6: Longhorn (REJECTED)
 * **Why it fails:** Longhorn does block management in userspace and is completely incompatible with ZFS's Copy-on-Write nature. Layering Longhorn on ZFS causes massive write amplification, destroying database performance. It also expects symmetric cluster storage, which the NUC lacks.

# LINSTOR + ZFS on Talos Linux Storage Architecture
## Executive Summary & True Intent
The goal is to replace a fragile Democratic-CSI + TrueNAS dynamic storage provisioning setup in a resource-constrained environment where Ceph is too heavy. The replacement must maintain ZFS data integrity (bitrot protection, snapshots) while supporting automated, pull-based off-site backups to a remote TrueNAS instance. The solution is to use **LINSTOR** on a **Talos Linux** cluster.
## Architectural Comparison & Hardware Evaluation
 * **Ceph:** Dropped due to high user-space resource consumption (~1–2GB RAM per 1TB data per OSD, high CPU overhead).
 * **Democratic-CSI:** Dropped due to configuration fragility. It only operates as a remote API caller over network protocols (iSCSI/NFS), adding friction to Kubernetes PV lifecycles.
 * **SeaweedFS:** Dropped as a 1:1 replacement. While lightweight, its open-source version lacks automated local bitrot scrubbing/self-healing (Enterprise only) and does not offer native, instant block-level snapshots like ZFS.
 * **LINSTOR:** Chosen as the ideal solution. It is a 100% open-source, lightweight control plane (~128MB–500MB RAM base) that interfaces directly with local storage engines (ZFS, LVM). It uses the built-in Linux kernel module **DRBD 9** for highly efficient, synchronous replication with minimal overhead (~32MB RAM per 1TB replicated data).
## Technical Quirks, Decisions & Discoveries
### 1. Appliance Conflict & Talos Constraints
 * **TrueNAS Separation:** LINSTOR requires root-level OS access and the DRBD kernel module to manage ZFS pools locally via its Satellite agent. Forcing this onto TrueNAS SCALE will break upon OS updates and confuse TrueNAS middleware. Therefore, TrueNAS must be entirely decoupled from local dynamic provisioning.
 * **Talos Immutable OS:** Talos Linux has no SSH, no root shell, and an immutable root filesystem. Running ZFS and LINSTOR requires configuring **Talos System Extensions** (zfs and drbd kernel modules) via Sidero Labs.
### 2. Multi-Node Topology with Single Storage Pool
 * **Single Node Constraints:** The target environment features a multi-node Kubernetes cluster but only *one* physical node containing the disks/ZFS pool.
 * **LINSTOR Handling:** Replicas are not mandatory. Setting linstor.csi.linbit.com/placementCount: "1" creates volumes natively on the single storage node.
 * **Network Access:** If a pod schedules on a diskless compute node, LINSTOR dynamically provisions a diskless DRBD client in the kernel, allowing the pod to access the volume over the network transparently. If the storage node dies, storage goes offline (acceptable risk due to robust off-site backups).
 * **Access Modes:** Native ReadWriteMany (RWX) is supported across multiple physical nodes via the LINSTOR CSI driver (v2.10+).
### 3. Maintaining Pull-Based Off-Site ZFS Backups
 * **The Problem:** The secure, off-site TrueNAS instance relies on SSH to execute zfs send/recv to pull data. Talos has no SSH daemon.
 * **The Solution (Privileged Backup Pod):** A privileged Ubuntu container must be deployed on the Talos storage node.
   * It mounts the host's /dev/zfs and ZFS datasets via hostPath.
   * It runs an SSH daemon and the zfsutils-linux package.
   * The SSH port is exposed via a NodePort or LoadBalancer.
   * The remote TrueNAS connects to this container port, allowing it to pull Zvol snapshots cleanly.
   * *Crucial Constraint:* The container's zfsutils-linux version must closely match the underlying Talos ZFS kernel module version to avoid compatibility errors.
 * **Local Tasks:** ZFS scrubbing and snapshot automation must be managed manually via cron/systemd inside a privileged container or handled by a tool like Sanoid/Syncoid, as the LINSTOR UI only manages LINSTOR resources, not core ZFS administrative tasks.
### 4. ZFS Pool Management & RAM Boundaries
 * **Pool Naming:** The existing ZFS pool (transfer) must be permanently renamed *before* installing LINSTOR. Changing a pool name later breaks LINSTOR's database mapping and risks orphaning Kubernetes PVs.
 * **Memory Management:** On standard Linux, the ZFS ARC will consume up to 50% or more of system RAM, clashing with Kubernetes workloads and triggering the Linux OOM killer.
 * **Talos ARC Hard Limit:** Because Talos lacks a standard shell, parameters must be passed natively to the kernel module via the Talos MachineConfig. The boundary must be set in bytes.
## Actionable Deployment Roadmap
```yaml
# Step 1: Add kernel module parameters to Talos MachineConfig (Example: 8GB ARC Max)
machine:
  kernel:
    modules:
      - name: zfs
        parameters:
          - zfs_arc_max=8589934592  # 8 GB in bytes
          - zfs_arc_min=2147483648  # 2 GB in bytes

```
 1. **Talos Image Generation:** Build the Talos boot asset with both zfs and drbd system extensions included.
 2. **Rename Local Pool:** Run a temporary privileged container to execute zpool import transfer <final_pool_name>.
 3. **Deploy LINSTOR via Helm:** Deploy the Piraeus Operator. Configure a StorageClass enforcing placementCount: "1".
 4. **Deploy Privileged Backup Pod:** Create an SSH-enabled Ubuntu container on the storage node with /dev/zfs mapped via hostPath. Ensure the ZFS userspace utility versions mirror the Talos kernel ZFS version.
 5. **Reconfigure TrueNAS Off-Site:** Update the remote TrueNAS SSH connection to target the new Kubernetes NodePort/LoadBalancer IP to resume pull-based ZFS replication tasks.

# LINSTOR vs. Democratic-CSI Decision & Postgres Tuning
## Architectural Decision: Retaining Democratic-CSI
A conscious decision was made to **reject LINSTOR and remain with Democratic-CSI**.
### The Core Conflict: Datasets vs. Zvols
 * **LINSTOR's Constraint:** LINSTOR is strictly a block storage orchestrator. Its replication engine (DRBD) operates at the kernel block layer, forcing it to exclusively use **Zvols** (zfs create -V). It cannot provision or manage native ZFS datasets.
 * **The Single-Node Reality:** Because this infrastructure relies on a single storage node with disk access, the multi-node synchronous replication advantages of LINSTOR provide zero benefit.
 * **The Penalty:** Shifting to LINSTOR would force all data (including media) into a rigid block layer. This eliminates the native file-level capabilities of ZFS (dynamic record sizing, cross-namespace sharing, and transparent scrubbing) while adding the overhead of a standard Linux filesystem nested inside a Zvol.
## Data Integrity & Performance Findings
### 1. The Scrubbing Blindness of Zvols
 * **Datasets (File Layer):** ZFS understands the file system. If bitrot or unrecoverable corruption occurs, a standard ZFS scrub outputs the exact human-readable file path (e.g., /pool/media/movie.mp4), making targeted data recovery from off-site backups trivial.
 * **Zvols (Block Layer):** To ZFS, a Zvol is a completely opaque block container of raw 1s and 0s. If a block corrupts inside a Zvol, a ZFS scrub can only identify the raw block address (e.g., block <0x80>). Pinpointing which file broke requires unmounting the volume and running a manual Linux filesystem check (fsck) from within a container.
### 2. Post-PR Tuning for PostgreSQL
Following a successful code contribution (PR) allowing custom block sizes via Democratic-CSI, the database storage layer will be strictly tuned using isolated Zvols over iSCSI:
 * **PGDATA (Main Data Storage):** Postgres natively reads/writes in **8K pages**. A dedicated StorageClass will enforce a **16K volblocksize** to optimize ZFS compression while preventing severe Read-Modify-Write (RMW) amplification.
 * **pg_wal (Write-Ahead Log):** The WAL is a strictly sequential, append-only firehose. It will be carved into a separate Zvol utilizing a **64K or 128K volblocksize**. This allows ZFS to lay down sequential writes in larger, metadata-efficient contiguous stripes.
 * *The Commit Trap:* While a 256K or 1M block size sounds ideal for streaming, Postgres forces an fsync down to the hardware on *every single transaction commit*. Pushing the Zvol block size to 256K+ creates massive write amplification during small commits, as ZFS would be forced to modify and rewrite a 256K block for a tiny 8K transaction.
### 3. Filesystem Selection for Zvols
When formatting a Zvol for database workloads, **ext4** is strongly preferred over xfs.
 * ZFS already handles heavy Copy-On-Write (COW), checksumming, and caching infrastructure underneath.
 * xfs is a highly complex filesystem that generates significant metadata overhead on top of ZFS.
 * ext4 acts as a simpler, thinner, and more lightweight translation layer, minimizing redundant journaling overhead and passing database pages directly to the ZFS block layer with lower latency.
## Actionable Stability Blueprint for Democratic-CSI
```yaml
# Target StorageClass Example for PostgreSQL Data
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: truenas-postgres-data-16k
provisioner: org.democratic-csi.iscsi
parameters:
  fsType: ext4
  # Enforced via custom patched parameters
  volblocksize: "16k" 

```
 1. **Lock TrueNAS Updates:** Treat TrueNAS as a static enterprise appliance. Never enable auto-updates, as TrueNAS API shifts can abruptly break the Democratic-CSI scraping/SSH automation layer.
 2. **Network Fencing:** Isolate iSCSI and NFS traffic onto an unrouted storage VLAN (or direct DAC connection) with Jumbo Frames (MTU 9000) enabled to insulate Kubernetes from network-induced "stale mount / stuck pod" iSCSI loop failures.
 3. **Hybrid Architecture:** Keep unstructured media/archives on native ZFS Datasets (recordsize=1M) shared via NFS, and transactional workloads (Postgres) on strictly bounded Zvols (volblocksize=16k) via iSCSI.



# Architecture Note: Cloud-Native ZFS Storage on Talos Linux
## 1. Core Problem & Constraints
 * **The Pain Point:** Shoving systemd and an SSH daemon into a privileged container just to make Democratic CSI work feels unmaintainable, brittle, and breaks Kubernetes design principles.
 * **Host Limitations:** The host OS is **Talos Linux**. It is immutable, API-driven, and has no package manager (apt/pacman), no shell, and no host-level daemons (like nfsd or nvmetcli). All storage management *must* happen inside Kubernetes.
 * **Hardware Scale:** Currently a **2-node cluster**.
### Hard Criteria
 1. **ZFS Datasets** with strict size quotas.
 2. **ZFS Zvols** exposed via **NVMe-oF** (TCP).
 3. **NFS** for shared network file systems (RWX).
 4. **No legacy fat containers** (no systemd in a pod).
## 2. Why Existing Solutions Failed Our Criteria
 * **Rook-Ceph:** Disqualified because a 2-node cluster cannot safely maintain Ceph quorum and data replication rules.
 * **Linstor / Piraeus:** Disqualified because it handles block storage (zvols) well but does not natively support or manage ZFS datasets.
 * **OpenEBS + NFS Server Provisioner:** Disqualified because standard NFS provisioners lack file system quota support.
 * **Democratic CSI (Standard Out-of-the-Box):** Disqualified because its generic drivers require an SSH target, forcing the user into the systemd-in-a-container hack on Talos.
## 3. The Proposed Cloud-Native Architecture
To remain completely Kubernetes-native, we drop the idea of a monolithic container and instead use the **Kubernetes Operator Pattern** backed by **Custom Resource Definitions (CRDs)**.
```
[ PVC Request ] ──> [ Custom CSI Controller ] 
                            │
                            ▼ (Creates)
                    [ ZfsShare CRD ] 
                            │
       ┌────────────────────┴────────────────────┐
       ▼ (Watched by Node-01 Pods)               ▼ (Watched by Node-02 Pods)
[ Node-01 ZFS Executor ]                  [ Node-02 ZFS Executor ]
   └─> Creates Dataset/Zvol                  └─> Ignored (Node mismatch)
[ Node-01 NFS/NVMe Daemons ]
   └─> Compiles local config & reloads

```
### Component Breakdown
### A. The Custom Resource Definition (ZfsShare)
Instead of a single, giant ConfigMap (which suffers from race conditions and API write conflicts), every single volume gets its own individual, tiny CRD object in the K8s API.
```yaml
apiVersion: storage.homelab.io/v1
kind: ZfsShare
metadata:
  name: pvc-dynamic-12345
spec:
  nodeName: talos-node-01    # Pins the volume to the correct physical hardware
  protocol: nfs              # or 'nvmeof'
  dataset: mypool/k8s/pvc-12345
  quota: 20G
  clientIP: "*"

```
### B. The CSI Controller (The Provisioner)
 * **What it does:** Forked from democratic-csi (using the zfs-local module as inspiration for ZFS management, and the freenas modules for consumer/mounting logic).
 * **How it works:** When a PVC is requested, it does **not** execute local commands or SSH into anything. It simply writes a new ZfsShare CRD to the Kubernetes API and returns the intended mount paths back to Kubernetes. It is 100% stateless.
### C. The Node Executor & Target Pods (DaemonSet)
Pinned via nodeSelector or standard DaemonSets to nodes containing physical ZFS pools. They watch the K8s API for ZfsShare objects where spec.nodeName matches their local node.
 1. **ZFS Local Executor:** A privileged container (with access to the host's /dev/zfs via the Talos ZFS system extension). It watches the API, sees a new CRD for its node, and runs zfs create -V or zfs create -o quota=X natively.
 2. **Stateless NFS Target Pod:** A pod running nfs-kernel-server and a sidecar script. The sidecar queries the K8s API for *all* NFS-based CRDs assigned to its specific node, compiles a fresh /etc/exports file in memory, and triggers exportfs -arv.
 3. **Stateless NVMe-oF Target Pod:** A pod running nvmetcli. It pulls all NVMe-oF CRDs for its node, compiles the config.json target layout, and runs nvmetcli restore.
## 4. Key Architectural Design Wins
 * **Zero SSH & Zero Systemd:** Containers use native ServiceAccounts to talk to the K8s API. No init system required; standard Kubernetes process monitoring handles container crashes cleanly.
 * **Future-Proof Multi-Node Scaling:** If a second node with its own zpool is added in the future, the architecture requires zero code changes. Node 2's target pods will simply ignore Node 1's CRDs based on the spec.nodeName field.
 * **No Local State (Etcd as Source of Truth):** If a storage node reboots, the target pods boot up empty, read their respective CRDs from the K8s API (etcd), and instantly re-export all datasets and zvols dynamically.
 * **Race-Condition Proof:** If a worker node tries to mount an NFS share before the NFS Target Pod finishes running exportfs, Kubernetes' native **eventual consistency** will temporarily fail the mount, back off for a few seconds, and retry until it succeeds automatically.


# Architecture Note: Refined Cloud-Native ZFS Storage on Talos Linux
## 1. Architectural Strategy
We decouple the **Storage Plane (ZFS Allocation)** from the **Network Plane (NFS/NVMe-oF Protocol Sharing)**.
 * The **CSI Controller** executes ZFS commands directly against the kernel via local device mounts.
 * The **Custom CRD (ZfsExport)** acts strictly as an "intent to share" network configuration payload.
 * The **Network Target Pods** are entirely stateless, reading the CRDs to configure network visibility.
This refinement drastically simplifies the custom code footprint by delegating 100% of the ZFS lifecycle (creation, quotas, sizing, snapshots) back to the thoroughly tested zfs-local logic inside Democratic CSI.
## 2. Updated Component Architecture
### A. The CSI Controller (Storage Engine)
 * **Placement:** Deployed as a Deployment or DaemonSet, strictly pinned via nodeSelector to the Talos node containing the physical ZFS pool.
 * **Privileges:** Mounted with the host's /dev/zfs and /dev/zvol utilizing the Sidero Labs Talos ZFS system extension.
 * **Volume Creation Flow:**
   1. Receives standard PVC request (e.g., 50GB NFS Dataset or 20GB NVMe-oF Zvol).
   2. Executes native ZFS commands locally inside the container (zfs create -o quota=50G pool/dataset or zfs create -V 20G pool/zvol).
   3. Once ZFS allocation succeeds, it automatically fires a Kubernetes API call to create a ZfsExport CRD defining how to share that path.
   4. Returns standard CSI connection context back to Kubernetes.
### B. The Custom Resource Definition (ZfsExport)
The CRD contains no storage-sizing parameters or quota settings. It exists solely to instruct the target pods on network access routing.
```yaml
apiVersion: storage.homelab.io/v1
kind: ZfsExport
metadata:
  name: pvc-dynamic-abcde
spec:
  nodeName: talos-node-01      # Identifies which node hosts the physical share
  protocol: nvmeof            # 'nfs' or 'nvmeof'
  path: /dev/zvol/pool/zvol   # The local path created by the CSI controller
  allowedIPs: "*"

```
### C. The Network Target Pods (DaemonSets)
Pinned to the storage nodes. Because they no longer execute ZFS binaries or calculate quotas, their operational scripts are incredibly lightweight (~50 lines of Python or Go code).
 * **NFS Target Pod:** Continually watches the API for ZfsExport records where spec.nodeName matches its host and protocol == nfs. It compiles a local /etc/exports configuration mapping the requested paths, writes it to a memory-backed file system (tmpfs), and executes exportfs -arv.
 * **NVMe-oF Target Pod:** Watches the API for ZfsExport records where spec.nodeName matches its host and protocol == nvmeof. It compiles a clean JSON target config, maps the /dev/zvol/... paths to the kernel fabric layer, and fires nvmetcli restore.
## 3. Design Wins & Effort Reduction
 * **Minimal Custom Code:** You do not need to write complex ZFS error parsing or volume lifecycle management into your custom code. Democratic CSI handles it natively via its built-in zfs-local modules.
 * **Dumb & Simple DaemonSets:** The network sharing pods require no special ZFS utilities (zfsutils-linux) or elevated storage capabilities. They function as simple API-to-Text configuration compilers.
 * **Native Snapshot & Resize Handling:** Because volume management remains inside the CSI controller code, features like volume expanding or snapshot restoration work out-of-the-box using the existing democratic-csi core codebase.
 * **Stateless Network Layer:** If the NFS or NVMe pods crash or the node reboots, they instantly reconstruct their configurations from Kubernetes etcd upon startup. No persistent configuration state is held locally on the node.

## 4. Handling Pre-Existing Datasets (Static Provisioning via Native NFS)
### The Concept
For pre-existing data (e.g., media libraries), we bypass the **CSI Controller (Storage Plane)** completely. This guarantees data safety by preventing accidental zfs destroy commands if a volume is deleted in Kubernetes.
While the custom architecture handles dynamically *exporting* the share, we simplify the consumption side by using Kubernetes' native, built-in NFS client instead of the custom CSI driver.
### The Workflow
**1. The Dataset (Host Level)**
The data already exists natively on the Talos host (e.g., pool/media/movies). No zfs create operations are needed.
**2. The Network Share (The CRD)**
The cluster admin manually applies a static ZfsExport CRD pointing to the existing path.
 * The custom **NFS Target Pod (DaemonSet)** reads this CRD and dynamically exports the path via exportfs -arv, treating it exactly the same as a dynamically provisioned volume.
**3. The Persistent Volume (The Native PV)**
Instead of using complex CSI definitions (volumeHandle, volumeAttributes), the admin creates a clean, native NFS Persistent Volume pointing to the share exported in Step 2.
```yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: pv-media-movies
spec:
  capacity:
    storage: 10Ti # Placeholder size; NFS doesn't strictly enforce this natively here
  accessModes:
    - ReadWriteMany # Crucial: Allows multiple pods (Plex, Radarr) to mount it!
  persistentVolumeReclaimPolicy: Retain # CRITICAL: Prevents K8s from deleting the underlying dataset
  nfs: # <--- Native K8s NFS Client (Bypasses the CSI Driver)
    server: 10.0.0.100 # The IP of the Talos node exporting the share
    path: /pool/media/movies

```
**4. The Consumers (The PVCs)**
Any application pod (e.g., Plex, Jellyfin) generates a standard PersistentVolumeClaim (PVC) requesting ReadWriteMany access. Kubernetes natively binds this PVC to the static PV and mounts the NFS share without requiring any custom CSI logic on the worker node.
### Why this Hybrid Approach Works
 * **Separation of Concerns:** The custom K8s Operator is still responsible for *creating* the NFS share on the Talos host, but Kubernetes natively handles *mounting* it.
 * **Simplicity:** Native NFS YAML is significantly cleaner and universally understood by standard Kubernetes clusters.
 * **Safety:** Using Retain on a static PV ensures your media library is completely immune to dynamic PVC lifecycle deletion events.

**Note:** this part has one slight conceptual error. Pvs cannot be bound by multiple pvcs. Hence a pv and pvc combination has to be created for every namespace we want to mount the share.


## 5. Dynamic Server Address Resolution (Avoiding Hardcoded IPs)
### The Challenge: The Danger of Hardcoded IPs
When a CSI driver provisions a volume, it passes a volume_context (containing the server address) back to Kubernetes. Kubernetes permanently bakes this address into the PersistentVolume (PV) object.
 * If this address is a hardcoded physical IP (e.g., 10.0.0.100), and the node's static DHCP lease ever changes, the PV permanently breaks.
 * If the address is the raw node hostname (e.g., talos-node-01), Kubernetes CoreDNS cannot resolve it natively, creating a fragile dependency on the external network router (e.g., OpenWrt) for DNS resolution.
### The Solution: Talos resolveMemberNames
To make storage routing 100% dynamic and resilient without relying on external routers, the architecture leverages Talos Linux's native internal DNS discovery mechanism.
By enabling resolveMemberNames, Talos intercepts DNS queries on the host OS and resolves cluster member hostnames to their IPs entirely locally using its internal cluster state.
### Implementation Steps
**1. Talos Machine Configuration**
Apply the following patch to the Talos machine.yaml for all nodes to enable internal member resolution:
```yaml
machine:
  features:
    hostDNS:
      enabled: true
      resolveMemberNames: true

```
**2. CSI Controller Modification (Downward API)**
Instead of reading the node's physical IP, the custom CSI Controller reads its own node's name via the Kubernetes Downward API.
```yaml
env:
  - name: NODE_NAME
    valueFrom:
      fieldRef:
        fieldPath: spec.nodeName

```
**3. The Provisioning Workflow**
 1. The CSI Controller creates the ZFS dataset/zvol locally.
 2. The Controller writes the ZfsExport CRD for the DaemonSets.
 3. The Controller builds the volume_context using NODE_NAME instead of an IP address:
   {"server": "talos-node-01", "share": "/pool/k8s/pvc-12345"}
 4. The Kubelet on the consumer node receives the context and runs:
   mount -t nfs talos-node-01:/pool/k8s/pvc-12345 ...
 5. The Talos host DNS intercepts the request, natively resolves talos-node-01 to the current static DHCP lease, and executes the mount.
### Architectural Benefits
 * **Future-Proof:** If a node's DHCP lease changes, Talos updates its internal state automatically. PVs never need to be patched or recreated.
 * **No External Dependencies:** The mount command does not rely on CoreDNS or the OpenWrt router to resolve the storage target.
 * **Immutable OS Friendly:** completely bypasses the need to manually hack /etc/hosts files across the cluster.


# ZFS Shares on Talos — Design Notes

## Context
- Custom Kubernetes operator (`zfs-shares`) that exposes ZFS datasets over **NFS** and ZFS zvols over **NVMe-oF (TCP)** on a Talos cluster.
- Architecture: privileged DaemonSet pods run the in-container kernel NFS server / nvmet target. A `ZfsShare` CRD carries "intent to share"; controllers render exports (NFS) or nvmet configfs (NVMe-oF) per node.
- Pool: classic `tank`, currently `mountpoint=/tank`.

## Problem 1 — `mountpoint=/tank` breaks on Talos
- Talos `/` is a **read-only, immutable** filesystem. ZFS can auto-create a mountpoint dir, but **cannot create a top-level `/tank`** because `/` isn't writable.
- Result: the pool ends up **imported but unmounted** on the host.
- Fix: move the mountpoint under the writable/persistent **var** tree, e.g. `/var/mnt/tank` (Talos-idiomatic) or `/var/lib/zfs/tank`.

## Problem 2 — Is hostPath into the container OK? (Yes)
- It's the intended pattern, not a hack: Talos has no host `nfsd`/`nvmetcli`, so the kernel NFS server runs **inside** a privileged pod, with the pool provided via `hostPath`.
- Critical flag already present: `mountPropagation: HostToContainer` — child datasets mounted on the host (each a separate mount) propagate into the pod. Without it, child datasets appear as empty dirs.
- Requires `privileged: true` (present) and rshared host mounts (Talos default).
- `hostPath type: Directory` means the mountpoint must exist when the pod starts (pool imported/mounted by Talos ZFS extension before kubelet launches the pod — normal boot order).
- NVMe-oF is unaffected — it uses `/dev/zvol/...` via the dev hostPath, not the dataset mountpoint.

## Problem 3 — Do child datasets inherit the mountpoint? (Yes)
- `zfs set mountpoint=/var/lib/zfs/tank tank` → every child with an **inherited** mountpoint mounts relative to the parent (e.g. `tank/k8s/pvc-123` → `/var/lib/zfs/tank/k8s/pvc-123`). ZFS unmounts/remounts affected datasets immediately.
- Caveats:
  - Datasets with an **explicit** mountpoint keep their own value (check: `zfs get -r mountpoint tank`).
  - Datasets set to `mountpoint=legacy` or `none` are unaffected.

## Critical — keep three paths identical
- The NFS controller writes `spec.Path` verbatim into exports and runs `exportfs -ra` inside the container.
- `spec.Path` is the dataset's ZFS mountpoint (a host path). For `exportfs` to succeed, that exact path must also be mounted **inside the container**.
- Therefore: **`hostPath` == `mountPath` == ZFS mountpoint**.
- Example chart values:
  ```yaml
  nfs:
    pool:
      hostPath: /var/lib/zfs/tank
      mountPath: /var/lib/zfs/tank
  ```
  and on host: `zfs set mountpoint=/var/lib/zfs/tank tank`.

## Observation — `zfs mount` in a debug container showed `/tank` in-container but not on host
Two behaviors with namespaces:
- **Pool import** (`zpool import`) = global kernel state, not namespaced.
- **Dataset mounts** (`zfs mount`) = per-mount-namespace VFS mounts.

What happened: host mount at `/tank` failed (read-only `/`), so pool was imported but unmounted. The debug container's `/` is writable, so `zfs mount` succeeded there — but that mount lived only in the container's namespace (default propagation doesn't push back to host). This just confirms the read-only-root issue from another angle.

## Approach comparison — mount in-container vs. host-mount + propagation
Key point: **for serving NFS, both are identical to nfsd.** The pod has its own mount namespace, and `exportfs`/`rpc.mountd`/`nfsd` all run inside the pod. Current design *propagates* host mounts in; the alternative *creates* them directly in the pod. Either way mounts live in the pod's namespace. No NFS-serving penalty either way. Decision is purely operational.

### Mount inside the container
- ✅ Sidesteps read-only root — no need to relocate mountpoint to var.
- ✅ Self-contained: agent owns import + mount + export; matches "reconstruct on restart" philosophy.
- ⚠️ Must coordinate with Talos ZFS extension (`zpool import -a` at boot): set datasets to **`canmount=noauto`** or **`mountpoint=legacy`** so host and pod don't fight.
- ⚠️ Agent must run `zpool import` / `zfs mount -a` idempotently and handle already-imported/already-mounted.
- ⚠️ Data visible **only inside that pod's namespace** — no host tooling, no separate backup/scrub pod can reach the filesystem (without sharing the namespace or its own mount).
- ⚠️ Mounts die with the pod; rebuilt on restart.

### Host-mounted + hostPath propagation (current design, with var mountpoint)
- ✅ Host = single source of truth; mounts survive pod restarts.
- ✅ Every consumer (backup pod, scrub jobs, other DaemonSets) can see the data.
- ✅ Conventional, least-surprising pattern.
- ⚠️ Requires moving the mountpoint under a writable tree (`/var/mnt/tank` or `/var/lib/zfs/tank`).

### Recommendation
- Lean toward **host-mount + var mountpoint**: keeps ZFS ownership on host (important once the pull-based backup pod exists), mounts survive restarts, least surprising.
- In-container mounting is legitimately clean for a pure NFS/NVMe gateway. If chosen, must-do: set `canmount=noauto` (or `mountpoint=legacy`) so Talos doesn't half-mount at boot, and add explicit import/mount logic to the agent — which today it lacks (`Server.Prepare()` only mounts nfsd and `rpc_pipefs`, never ZFS; it assumes datasets arrive via host propagation).

# ZFS Shares on Talos — Mount, Propagation & Kubelet Findings

## Context
- Custom Kubernetes operator (`zfs-shares`) exposing ZFS datasets over **NFS** and zvols over **NVMe-oF (TCP)** on a Talos cluster.
- Privileged DaemonSet pods run the in-container kernel NFS server / nvmet target. A `ZfsShare` CRD carries "intent to share"; controllers render exports (NFS) or nvmet configfs (NVMe-oF) per node.
- Pool: classic `tank`. Decision: change pool mountpoint to `/var/mnt/tank`.
- Helm chart stays **generic** (source `hostPath` + destination `mountPath` kept separate — standard Kubernetes volume semantics). All ZFS/Talos specifics live in node config, not the chart.

## Problem: `mountpoint=/tank` breaks on Talos
- Talos `/` is a **read-only, immutable** filesystem. ZFS can't create a top-level `/tank` dir at the root, so the pool ends up **imported but unmounted** on the host.
- Fix: move the mountpoint under the writable/persistent **var** tree, e.g. `/var/mnt/tank`.
- `zfs set mountpoint=/var/mnt/tank tank`.

## Child datasets inherit the mountpoint
- `zfs set mountpoint=/var/mnt/tank tank` → every child with an **inherited** mountpoint mounts relative to the parent (e.g. `tank/pvc-123` → `/var/mnt/tank/pvc-123`). ZFS unmounts/remounts affected datasets immediately.
- Caveats:
  - Datasets with an **explicit** mountpoint keep their own value (`zfs get -r mountpoint tank`).
  - Datasets set to `mountpoint=legacy` or `none` are unaffected.

## hostPath into a privileged container is the correct pattern
- Talos has no host `nfsd` / `nvmetcli`, so the kernel NFS server runs **inside** a privileged pod, pool provided via `hostPath`.
- NVMe-oF is unaffected by mountpoint choices — it uses `/dev/zvol/...` via the dev hostPath, not the dataset mountpoint.

## Why the debug container saw `/tank` but the host didn't
- **Pool import** (`zpool import`) = global kernel state, not namespaced.
- **Dataset mounts** (`zfs mount`) = per-mount-namespace VFS mounts.
- Host mount at `/tank` failed (read-only `/`) → pool imported but unmounted. The debug container's `/` is writable, so `zfs mount` succeeded there — but only in that container's namespace (default propagation doesn't push back to host).

## The export-path identity rule
- The NFS controller writes `spec.Path` **verbatim** into exports and runs `exportfs` **inside the container**.
- `spec.Path` only has to resolve **inside the NFS container**; the host path is irrelevant to NFS export resolution.
- Correct invariant:
  ```
  spec.Path == path inside CSI container == path inside NFS container
  ```

## The critical ZFS mechanic
- **ZFS mounts a dataset at its `mountpoint` *property* value**, in whatever namespace runs the mount — it ignores where you bind-mounted the pool.
- `zfs get mountpoint` also returns the property (a host-style path).
- Consequence: the container `mountPath` must equal the ZFS `mountpoint` property, otherwise ZFS mounts at the property path (outside the bind-mounted subtree) and nothing lines up / nothing propagates.

## Mount propagation: will the NFS pod see a dataset the CSI pod creates?
- Propagation only carries mounts that occur **inside the propagated (bind-mounted) subtree**.
- **Case 1 — container path == host mountpoint (both `/var/mnt/tank`): ✅ works**
  1. CSI pod runs `zfs create` → ZFS mounts at `/var/mnt/tank/pvc-123` inside its shared bind subtree.
  2. CSI pod mount is `Bidirectional` (rshared) → event propagates **out** to the host peer group.
  3. NFS pod mount is `HostToContainer` (rslave) → mount propagates **in** to the NFS pod.
  4. `spec.Path = /var/mnt/tank/pvc-123`, `exportfs` finds it.
- **Is it impossible due to propagation? No** — `Bidirectional` is exactly the mechanism. Plain one-way `HostToContainer` on the **CSI** side would fail (never flows back out). NFS side only receives, so `HostToContainer` is fine there.
- **Case — containers match each other but differ from host (both `/tank`, host `/var/mnt/tank`): ❌ fails**
  - ZFS still mounts at the property path `/var/mnt/tank/pvc-123`, which is outside the containers' `/tank` bind subtree → no propagation. Only fixable by also setting the property to `/tank` (not wanted).

## Working recipe (given `mountpoint=/var/mnt/tank`)
- `zfs set mountpoint=/var/mnt/tank tank` (host mounts it there — writable, no RO-root problem).
- **Both** containers: `hostPath: /var/mnt/tank`, `mountPath: /var/mnt/tank` (equal to the property).
- CSI container: `mountPropagation: Bidirectional`.
- NFS container: `mountPropagation: HostToContainer`.
- `spec.Path = /var/mnt/tank/<dataset>`.

## Propagation requirements summary
For the NFS pod to see a CSI-created dataset, ALL must hold:
1. CSI pod pool mount = `Bidirectional` (mounts flow out).
2. NFS pod pool mount = `HostToContainer` or `Bidirectional` (mounts flow in).
3. The new ZFS mount lands **inside** the shared subtree ⇒ ZFS `mountpoint` property == container bind path.
4. The host's pool mount is **shared (rshared)** — on Talos, established via the kubelet `extraMount` (below).

## Talos kubelet `extraMounts` — why and whether it's required
- On normal distros the kubelet runs in the host mount namespace and `/` is already `rshared` (systemd), so hostPath + propagation just work.
- On Talos the kubelet runs **as a container with its own mount namespace** and only sees explicitly mounted host paths. Two reasons the `extraMount` is needed:
  1. **Visibility:** hostPath volumes are wired up from the kubelet's view. `/var/mnt/tank` is not a default kubelet mount → without the `extraMount`, pods get an **empty directory**.
  2. **Propagation:** the chain host → kubelet → pod must be `rshared` for dynamically-created ZFS mounts to reach pods (`HostToContainer`) and for CSI-created mounts to propagate out (`Bidirectional`).
- **Required in this setup? Yes** — pool is at a non-default path and needs both visibility and propagation.
- This lives in the Talos `MachineConfig` on the storage node(s), NOT in the Helm chart.

```yaml
machine:
  kubelet:
    extraMounts:
      - destination: /var/mnt/tank
        type: bind
        source: /var/mnt/tank
        options:
          - rbind      # recursive: pull in already-mounted child datasets
          - rshared    # propagate mount/unmount events both ways
          - rw
```

- `rbind` = recursive bind so existing nested dataset mounts come along.
- `rshared` = puts the kubelet's view in the same propagation peer group as the host.
- Verify on a node: `findmnt -o TARGET,PROPAGATION /var/mnt/tank` should report `shared`.
- Symptom if missing: pod's `/var/mnt/tank` is empty, or newly-created datasets never appear in the NFS export.

## Alternative considered: mount ZFS inside the container
- Viable and sidesteps the RO-root problem; for NFS serving it's identical to the host-mount approach (nfsd/mountd/exportfs all run in the pod namespace either way).
- Downsides: must coordinate with Talos ZFS extension (`canmount=noauto` / `mountpoint=legacy` to avoid double-mount), agent must run `zpool import` / `zfs mount` idempotently, data visible only inside the pod namespace (no host tooling / separate backup/scrub pod access), mounts die with the pod.
- Chosen direction instead: host owns the mounts at `/var/mnt/tank` (single source of truth, survives pod restarts, visible to other tooling).


## 7. Storage Nodes vs. Consumer Nodes: Kubelet and extraMounts

### The Core Distinction

When configuring Talos for this storage architecture, Kubelet behaves completely differently depending on the node's role. A strict distinction must be made between **Storage Nodes** (hosting the physical ZFS disks and CSI/NFS DaemonSets) and **Consumer Nodes** (hosting the application pods like Plex or Radarr).

### 1. Consumer Nodes (Application Nodes)

**Do they need Talos `extraMounts`?** **NO.**

* On nodes where standard applications run, Kubelet does not need host-level access to the ZFS dataset.
* When a pod claims a volume, Kubelet simply executes a network mount (`mount -t nfs <IP>:/var/mnt/tank/pvc-123`).
* Because this happens entirely over the network stack (often hairpinned via Cilium eBPF), no host paths, bind mounts, or Talos configuration patches are required on consumer nodes.

### 2. Storage Nodes (CSI & NFS DaemonSet Nodes)

**Do they need Talos `extraMounts`?** **YES.**

* Because Talos runs the Kubelet service inside its own isolated container/mount namespace, Kubelet cannot natively see the host OS's `/var/mnt/tank` directory.
* If the `extraMount` is missing, Kubernetes will pass an empty directory to the CSI and NFS pods when they request a `hostPath` volume, breaking the entire provisioning and sharing pipeline.

### The Propagation Chain (Why `rshared` is mandatory)

For the NFS Pod to instantly see a dynamically created dataset from the CSI Pod, the mount event must propagate through multiple isolation boundaries. The Kubelet acts as the critical middleman.

The mount event follows this exact chain:

1. **CSI Pod** executes `zfs create`.
2. Passes through the CSI Pod's `Bidirectional` volume mount to...
3. **The Kubelet's Mount Namespace**.
4. Passes through the Kubelet's `rshared` extraMount boundary to...
5. **The Talos Host OS**.
6. Passes *back down* through the Kubelet's `rshared` boundary to...
7. **The Kubelet's Mount Namespace**.
8. Passes through the NFS Pod's `HostToContainer` volume mount to...
9. **The NFS Pod**, allowing `exportfs` to detect and share the new path.

If the Talos Kubelet `extraMount` lacks `rshared`, the event is permanently trapped in the CSI Pod/Kubelet layer and never reaches the host or the NFS server.

### Talos MachineConfig for Storage Nodes

This block must be present in the `machine.yaml` of any node physically hosting the ZFS pool and running the storage DaemonSets:

```yaml
machine:
  kubelet:
    extraMounts:
      - destination: /var/mnt/tank
        type: bind
        source: /var/mnt/tank
        options:
          - rbind      # recursive: pull in already-mounted child datasets
          - rshared    # critical: propagates mount/unmount events both ways across Kubelet boundary
          - rw

```


## 8. Network & Metrics Strategy (hostNetwork & Prometheus)
### The hostNetwork Requirement
For the storage Data Plane (NFS and NVMe-oF), hostNetwork: true is strictly required.
 * **Performance (The CNI Tax):** Routing storage traffic through the Kubernetes overlay network (CNI) forces packet encapsulation/decryption, severely degrading throughput and spiking CPU usage.
 * **NVMe-oF Routing:** NVMe-over-TCP relies on strict, predictable kernel-level routing. Abstracting the initiator connection behind a virtual Pod IP breaks seamless reconnection if a link flaps.
### Port Collision Management
Running on hostNetwork introduces the risk of port collisions. The industry standard is to assign default ports and expose them as configurable environment variables.
 * **Avoid Standard Ports:** Do not use 10250 (Kubelet), 9100 (Node Exporter), or 9090/9962 (Cilium).
 * **Selection:** Use unassigned blocks from the official Prometheus Wiki (e.g., 9881 for NFS metrics, 9882 for NVMe-oF metrics).
 * **Best Practice:** Register the chosen ports on the Prometheus Default Port Allocations GitHub Wiki to reserve them publicly.
### Prometheus Dynamic Discovery
Scraping host-networked pods does not require hardcoding physical Node IPs into Prometheus. The Prometheus Operator natively handles discovery via standard Kubernetes abstractions.
 1. **The Headless Service:** Create a Service with clusterIP: None. Kubernetes will automatically map the physical Node IPs of the hostNetwork pods into the Service's Endpoints object.
 2. **The ServiceMonitor:** The ServiceMonitor watches the Service. The Prometheus Operator reads the dynamically generated Endpoints list and automatically configures scrapes directly against the physical Node IP.
```yaml
# 1. Headless Service
apiVersion: v1
kind: Service
metadata:
  name: zfs-shares-metrics
spec:
  clusterIP: None
  selector:
    app: zfs-shares
  ports:
    - name: metrics-nfs
      port: 9881
    - name: metrics-nvme
      port: 9882

# 2. ServiceMonitor
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: zfs-shares-monitor
spec:
  selector:
    matchLabels:
      app: zfs-shares
  endpoints:
    - port: metrics-nfs
    - port: metrics-nvme

```
## 9. NFS Architecture: In-Kernel vs. User-Space
### The Talos Initialization Constraint
Talos Linux provides the nfsd kernel modules but completely lacks host-level user-space tools (no systemd, rpcbind, exportfs, or mountd). Therefore, the privileged DaemonSet container must act as the initialization system. The container's entrypoint must manually orchestrate the legacy Linux services before sleeping:
```bash
#!/bin/sh
rpcbind                  # 1. Start portmapper
rpc.statd --no-notify    # 2. Start state daemon
rpc.mountd               # 3. Start mount daemon
exportfs -arv            # 4. Feed exports to kernel
rpc.nfsd 8               # 5. Spin up kernel threads
exec sleep infinity      # 6. Keep container alive

```
### In-Kernel NFS vs. NFS-Ganesha
 * **In-Kernel (nfsd):** Maximum bare-metal performance. The kernel reads ZFS blocks directly into kernel memory and sends them out the network interface. Data never leaves kernel space. Requires kernel modules loaded via Talos machine.yaml and manual daemon orchestration (above).
 * **User-Space (NFS-Ganesha):** A single binary (ganesha.nfsd) that handles the entire protocol in user-space, bypassing rpcbind and kernel dependencies. Uses FSAL_VFS to read standard POSIX mounts.
 * **The Trade-off:** Ganesha is easier to containerize but incurs a heavy performance penalty for local ZFS storage due to constant context switching (Disk → Kernel → User-Space Ganesha → Kernel → Network). In-kernel is the optimal choice for high-throughput ZFS architecture.
## 10. NVMe-oF Target Architecture
### In-Kernel (nvmet) vs. User-Space (SPDK)
Unlike NFS, user-space NVMe-oF (via Intel SPDK) is actually *faster* than the Linux kernel implementation, capable of millions of IOPS per core by bypassing standard kernel interrupts and utilizing 100% CPU polling.
However, **SPDK is fundamentally incompatible with a ZFS-backed architecture.**
 * SPDK requires direct, exclusive access to physical flash memory hardware.
 * ZFS is a massive kernel module. zvols (ZFS block devices) exist entirely in kernel space.
 * Attempting to pipe a kernel-space zvol up into user-space SPDK just to wrap it in NVMe-oF protocol creates an extreme architectural bottleneck.
### The Winning Strategy: In-Kernel nvmet-tcp
Because ZFS zvols already live in the Linux kernel, using the kernel's native nvmet-tcp subsystem keeps the entire data path localized to kernel-space.
 1. Talos Linux provides the nvmet and nvmet-tcp kernel modules natively.
 2. The custom DaemonSet simply mounts /sys/kernel/config via hostPath.
 3. The controller dynamically writes text configuration to /sys/kernel/config/nvmet to map the zvols to target namespaces, streamlining the entire storage pipeline with zero user-space translation overhead.


## 11. Storage Data Plane Client Architecture & Cilium Network Policy
### Mount Execution & Context
The application container does not make the direct network connection to the storage target. Instead, the connection is initiated by the host kernel on behalf of the node's Kubelet.
 * **Orchestration:** When a pod requests a volume, Kubelet calls the **CSI Node Plugin** DaemonSet running on that client node.
 * **The Mount/Connect Commands:** The plugin issues standard kernel-level client commands:
   * **NFS:** mount -t nfs <target-ip>:/share /var/lib/kubelet/pods/...
   * **NVMe-oF:** nvme connect -t tcp -a <target-ip> -n <nqn> ...
 * **Network Namespace:** Because these are kernel-level modules, the TCP connection is routed through the **Host Network Namespace**. The traffic completely bypasses the Cilium CNI overlay network. The application container is completely unaware of the network layer and simply interacts with a local bind-mount injected into its namespace by Kubelet.
### Traffic Identity
Because all storage traffic originates from the host kernel, the storage targets (the storage node DaemonSets) will only ever see incoming packets labeled with the **physical IP addresses of the Talos worker nodes**. Pod CIDR IPs (e.g., 10.244.x.x) are never used for the storage data plane.
### Cilium Network Policy Configuration
To secure the data plane, Cilium policies must be configured to allow ingress traffic exclusively from the physical node subnet or Cilium's native host/node entities.
```yaml
apiVersion: "cilium.io/v2"
kind: CiliumClusterwideNetworkPolicy
metadata:
  name: "secure-storage-dataplane"
  namespace: storage-system
spec:
  endpointSelector:
    matchLabels:
      app: zfs-shares  # Targets the storage server DaemonSets
  ingress:
    # Option A: Restrict to physical node network CIDR
    - fromCIDR:
        - "192.168.10.0/24"
      toPorts:
        - ports:
            - port: "2049"  # NFS
              protocol: TCP
            - port: "111"   # rpcbind
              protocol: TCP
            - port: "4420"  # NVMe-oF Default Port
              protocol: TCP
              
    # Option B: Restrict via Cilium's native entity tags
    - fromEntities:
        - remote-node
        - host
      toPorts:
        - ports:
            - port: "2049"
              protocol: TCP
            - port: "111"
              protocol: TCP
            - port: "4420"
              protocol: TCP

```
### Application-Level Access Control
Network policies must be paired with application-level restrictions:
 * **NFS Exports:** Write your /etc/exports profiles to allow the host subnet rather than wildcard targets:
   /var/mnt/tank/share1 192.168.10.0/24(rw,sync,no_root_squash)
 * **NVMe-oF Targets (nvmet):** Configure the allowed_hosts directory inside /sys/kernel/config/nvmet/ to explicitly match only the specific Host NQNs of the authorized client Talos nodes.
   

## 18. Storage Node Health & Dead Node Resolution (Detailed Architecture)
### 18.1 The Fallacy of Self-Reporting
A completely dead node (power loss, kernel panic, or motherboard failure) cannot clean up its own Custom Resource Definition (CRD) or report its status as OFFLINE. Its local discovery DaemonSet dies with it, leaving the ZfsPool CRD falsely claiming the pool is ONLINE at a dead IP.
### 18.2 Two-Tier Monitoring Architecture
To handle all failure modes securely without false positives, the operator uses a two-tier monitoring architecture.
#### Tier 1: Local DaemonSet (Hardware/ZFS Health)
 * **Scope:** Runs as a privileged DaemonSet on all live storage nodes.
 * **Role:** Monitors the pool's internal zpool health while the host operating system is still breathing.
 * **Status Updates:** Reports localized physical array failures to the ZfsPool CRD:
   * ONLINE: Pool is healthy.
   * DEGRADED: A drive in the array failed, but the pool is still serving I/O.
   * FAULTED: Too many drives failed; the pool is locked.
   * SUSPENDED: Host Bus Adapter (HBA) crash or detached cables; I/O is paused.
#### Tier 2: Central Watcher (Node Death)
 * **Scope:** Standard Kubernetes Deployment (1 replica) running anywhere in the cluster.
 * **Role:** Detects cluster-level node outages and overrides stale CRD statuses.
 * **The Reconciliation Loop:**
   1. Continually watches core Kubernetes Node objects.
   2. Detects when a physical node transitions to Ready: False (or NotReady after the standard Kubelet timeout).
   3. Queries the Kubernetes API for all ZfsPool CRDs where status.currentNode == <Dead_Node_Name>.
   4. Forcibly updates those specific CRDs to status.health: NODE_OFFLINE.
### 18.3 The Custom Resource Definition (CRD) Structure
To make this work, the custom ZfsPool resource is designed to separate the human-readable name from the globally unique identifier, while tracking network routing state.
#### The ZfsPool CRD Schema Overview
 * **Metadata Name:** Must be the immutable ZFS Pool GUID (e.g., zpool-12140134988506841113) to prevent name collisions across nodes.
 * **Spec:** Declares the human-readable name and configuration.
 * **Status:** Holds the dynamic routing and health data updated by the Operator components.
#### Example 1: Healthy State (Written by Local DaemonSet)
When the node is operating normally, the local DaemonSet patches the status field:
```yaml
apiVersion: storage.homelab/v1
kind: ZfsPool
metadata:
  name: zpool-12140134988506841113
spec:
  poolName: tank
status:
  currentNode: worker-a
  currentIP: 192.168.10.15
  health: ONLINE
  lastUpdated: "2024-05-12T10:00:00Z"

```
#### Example 2: Dead Node State (Overwritten by Central Watcher)
When worker-a loses power, the Central Watcher detects the NotReady node state and patches the CRD to prevent the CSI driver from hanging:
```yaml
apiVersion: storage.homelab/v1
kind: ZfsPool
metadata:
  name: zpool-12140134988506841113
spec:
  poolName: tank
status:
  currentNode: worker-a       # Kept for historical reference
  currentIP: 192.168.10.15    # Kept for historical reference
  health: NODE_OFFLINE        # <--- Forcibly updated by Watcher
  lastUpdated: "2024-05-12T10:05:30Z"

```
### 18.4 CSI Driver Impact & Recovery
 1. **Immediate Failure:** By using the Central Watcher, the CSI Node Plugin does not hang trying to establish a TCP/NVMe connection to a non-responsive IP. If the CRD status is NODE_OFFLINE, the plugin immediately halts the mount attempt and returns a clean gRPC error: *"Mount failed: Storage node is offline."*
 2. **The Takeover (Recovery):** Once the physical disks are pulled from the dead node and imported on a healthy node, the new node's Local DaemonSet detects the GUID. It immediately overwrites the NODE_OFFLINE state, updating currentNode, currentIP, and setting health back to ONLINE. The CSI driver seamlessly resumes mounting using the new target IP.


## 19. NFS Path Abstraction & Pool Renaming Resilience
### 19.1 The Problem with Hardcoded NFS Paths
Standard NFS PersistentVolumes hardcode the absolute export path (e.g., /tank/pvc-12345) into the Kubernetes PV object. If the ZFS pool is renamed to watertank, or imported on a new node using an alternate root path (e.g., zpool import -R /mnt tank), the absolute path changes on the host OS. The hardcoded PV permanently breaks because it points to a path that no longer exists.
### 19.2 ZFS Server-Side Native Resilience
When a dataset is shared via the ZFS native sharenfs property, ZFS dynamically manages the kernel NFS exports. If a pool is renamed or its mountpoint shifts, ZFS automatically updates the NFS export path on the server OS in real-time. The server naturally tolerates path changes; the architectural challenge is entirely about routing the CSI client.
### 19.3 The CRD Extension (Dynamic Pathing)
To make the CSI driver immune to path changes, the Late Binding pattern is extended to track file paths. The Local Discovery DaemonSet executes zfs get -H -o value mountpoint <pool_name> and publishes this to the ZfsPool CRD as status.baseMountPath.
**Example CRD State:**
```yaml
apiVersion: storage.homelab/v1
kind: ZfsPool
metadata:
  name: zpool-12140134988506841113  # The immutable ZFS GUID
spec:
  poolName: watertank               # Can be renamed safely
status:
  currentNode: worker-b
  currentIP: 192.168.10.55
  baseMountPath: /mnt/watertank     # <--- The dynamic root path
  health: ONLINE

```
### 19.4 Controller Phase (Logical Identity)
During the CreateVolume gRPC phase, the CSI Controller must **never** write the absolute path into the VolumeContext. It only writes the logical dataset name (usually the PVC ID) and the Pool GUID.
```go
// Controller Plugin VolumeContext Injection
"protocol":  "nfs",
"pool_guid": "12140134988506841113",
"dataset":   "pvc-12345", // Logical folder name only

```
### 19.5 Real-Time Path Construction (Node Plugin)
When Kubelet triggers the mount on the application node, the CSI Node Plugin queries the Kubernetes API to fetch the CRD in real-time. It retrieves the IP and the baseMountPath, concatenates them with the logical dataset name, and dynamically executes the mount.
```go
// 1. Fetch real-time data from the ZfsPool CRD via API
targetIP := zfsPoolObj.Status.CurrentIP            // "192.168.10.55"
basePath := zfsPoolObj.Status.BaseMountPath        // "/mnt/watertank"

// 2. Fetch logical identity from the K8s PV Context
datasetName := request.VolumeContext["dataset"]    // "pvc-12345"

// 3. Construct the absolute path dynamically
fullExportPath := filepath.Join(basePath, datasetName) // "/mnt/watertank/pvc-12345"

// 4. Execute standard mount
cmd := fmt.Sprintf("mount -t nfs %s:%s /var/lib/kubelet/pods/...", targetIP, fullExportPath)
RunCommand(cmd)

```
**Result:** The cluster survives node deaths, IP changes, pool renames, and mountpoint shifts without modifying a single Kubernetes PersistentVolume object.