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