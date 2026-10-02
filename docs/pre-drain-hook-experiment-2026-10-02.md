# Pre-drain hook experiment — machine-deletion hook semantics (2026-10-02)

Round-5 follow-up. Validates whether CAPI machine-deletion hooks gate the *infra* (Harvester VM)
deletion in the roll2 environment, and what that means for the controller's sequencing design.

## Question

Rounds 1–4 evidence (roll1, Rancher **2.14.1** mgmt): the machine controller deleted the infra VM
in parallel and hook-blind — the controller's `pre-terminate` hook gated only the machine object's
own finalization, and the VM died at **+8/+23/+32s** while the hook was held. Open question: does
registering a `pre-drain.delete.hook.machine.cluster.x-k8s.io/…` annotation block the infra
deletion too? If yes, the controller gains a deterministic gate (hold pre-drain → relocate →
release) and no longer depends on the fleet-side `drainBeforeDelete` flag. If no, the question is
closed and the fleet flag stands as the sequencing control.

## Environment

- roll2 = `c-m-pbf2xftn` (3 CP + 3 workers, RKE2 v1.33.13+rke2r1) on hobbyfarm (Harvester 1.7.1),
  mgmt = **Rancher HA 2.14.3**, Longhorn 1.12.x with 4 test volumes (3× 3-replica + 1× single-replica)
- Machine controllers under test: rke-machine machine controllers on the mgmt cluster
- Live pool flags: **workers** `drainBeforeDelete=true` (timeout 1h), **control-plane** absent
- Hooks registered before each deletion (same prefix family as the controller's own
  `pre-terminate.delete.hook.machine.cluster.x-k8s.io/longhorn-node-eviction`, timestamp value):
  - `pre-drain.delete.hook.machine.cluster.x-k8s.io/hook-experiment=<RFC3339>`
  - `pre-terminate.delete.hook.machine.cluster.x-k8s.io/hook-experiment=<RFC3339>`

## Experiment A — worker pool (flag ON)

Machine `lhcc-roll2-workers-crpdq-qvvdr` (no volume engines, no sole-replica exposure, no app pods).
Both hooks registered before delete. Times UTC.

| t | event |
|---|---|
| 14:02:03 | both experiment hooks registered |
| 14:02:18 | `kubectl delete machine`; deletionTimestamp set |
| 14:02:19–20 | lhcc controller: registers its own pre-terminate hook → triggers LH eviction → deletes departing-node replica CRs → "degraded-but-safe" (every volume keeps ≥1 ready replica elsewhere) → removes its hook (event `LonghornEarlyRelease`); node cordoned by the controller; LH node `evictionRequested=true` |
| 14:02:20 → 14:08:28 | machine condition `Deleting=True/WaitingForPreDrainHook` — hard hold 6m10s: drain never starts (12 pods constant), VM Running, VMI present, HarvesterMachine untouched |
| 14:08:28 | pre-drain hook released |
| 14:08:28 → 14:10:26 | drain runs (pods 12 → 6, DaemonSets only); `SuccessfulDrainNode` |
| ≤ 14:11:08 | machine condition `Deleting=True/WaitingForPreTerminateHook` — infra deletion held, VM still Running |
| 14:14:23 | pre-terminate hook released |
| 14:14:31 | VM deletionTimestamp **+8s** after release; `InfrastructureReady` → False |
| ~14:16–14:19 | replacement worker `psjmw` built + joined (~3 min); machine finalized |
| 14:22:5x | LH rebuilt: psjmw 3 / wpr49 3 / xfl4x 4 ready replicas; 4/4 volumes healthy |

Hop ≈ 20.5 min. Writers untouched, 0 restarts, zero data loss.

## Experiment B — control-plane pool (flag ABSENT)

Machine `lhcc-roll2-control-plane-hrmvj-rmb85` (CP nodes carry **no** Longhorn Node CR).
Note: CP machines normally carry a third hook, `pre-terminate…/rke-bootstrap-cleanup`, owned by
rke-bootstrap-controller; that controller removed it mid-deletion (harmless — distinct hook name;
CAPI requires *all* hooks released to pass a gate).

| t | event |
|---|---|
| 14:23:22 | both experiment hooks registered |
| 14:23:40 | delete |
| 14:23:41 | lhcc controller: registers its hook, finds **no Longhorn Node CR** → no-op release ("assuming Longhorn not installed or already cleaned up") |
| 14:23:40 → 14:28:05 | machine condition `Deleting=True/WaitingForPreDrainHook` — hold 4m25s: VM Running, no drain (CP pool has none), API healthy, etcd quorum 3/3 intact throughout |
| 14:28:05 | pre-drain hook released |
| ~14:29:00 | volume-detach stage (~55s); condition → `WaitingForPreTerminateHook` |
| 14:29:36 | pre-terminate hook released (gate held 36s, VM Running the whole time) |
| ≤ 14:30:09 | VM + VMI gone (≤ 33s after release); `InfrastructureReady` → False |
| ~14:33 | replacement CP `rmvlx` Ready; etcd back to 3 members |

Hop ≈ 9.5 min. LH volumes healthy throughout (no replicas on CP).

## Verdict (Rancher 2.14.3 mgmt)

1. **Pre-drain hooks are honored in both regimes** (flag ON and flag ABSENT): condition
   `Deleting=True/WaitingForPreDrainHook`; drain does not start; infra untouched for the entire hold.
2. **Pre-terminate hooks are honored where the flow reaches that stage**: infra deletion waits
   (`WaitingForPreTerminateHook`) with the VM alive at the gate in both regimes.
3. Release → infra deletion begins within seconds (+8s worker, ≤33s CP). Hook causality is clean —
   the machine controller proceeds the instant the last gate clears.
4. Deletion sequence on 2.14.3: **pre-drain gate → [drain (workers, flag on) | volume-detach (CP,
   flag off)] → pre-terminate gate → infra deletion → finalization.**
5. The hook-blindness of rounds 1–4 did **not** reproduce. The one correlated variable is the mgmt
   version (2.14.1 → 2.14.3); the 2.14.1 environment is decommissioned so this was not re-tested —
   treat rounds-1-4 behavior as version-specific until proven otherwise.
6. Untested but likely: a flag-less *worker* deletion follows the same gate order as the CP run
   (shared machine controller logic); only the CP datapoint is direct.

## Design implications for lhcc

1. **A pre-drain hook is a viable deterministic gate on 2.14.3**: register
   `pre-drain.delete.hook.machine.cluster.x-k8s.io/<name>` at machine-delete detection and release
   only after LH safety is established (eviction complete, ≥1 surviving data replica per affected
   volume). This gates drain + infra regardless of the fleet `drainBeforeDelete` flag (proven on
   both pools; flag-less worker combination untested but same controller path).
2. **The gate is belt-and-suspenders, not a replacement.** On 2.14.1 hook semantics appear weaker
   (pre-terminate ignored for infra). The controller's always-on path — early eviction: cordon +
   `evictionRequested` + replica-CR cleanup + degraded-but-safe check — is what actually protects
   data and must stay primary. The pre-drain hook would make sequencing *explicit* and robust.
3. **Release ordering matters**: only release the pre-drain hook after the safety check passes;
   after release the machine controller resumes immediately and owes the controller nothing.
4. **The controller's early-release composes cleanly with held gates**: in both runs it released
   its own hook in ≤2s (worker: eviction done; CP: no Longhorn Node CR) while the experiment gates
   still held the flow — no interference, no re-registration churn.
5. **Non-Longhorn guests**: the "Longhorn Node CR not found → no-op release" path is the natural
   guard for guest clusters without Longhorn (GKE PD CSI, AKS, Portworx) — the controller already
   no-ops on machines whose node has no Longhorn Node CR. Worth verifying once against a
   non-Longhorn guest cluster; no Longhorn-specific API calls should fire in that path.

## Next step (proposed, not implemented)

Add pre-drain hook registration to the controller: on machine-delete detection, register the hook
before anything else; perform eviction; verify safety; release. Keep the pre-terminate hook and the
90m eviction timeout as the finalization fallback. Config flag for environments where the mgmt
version is known to ignore hooks.

## Evidence

- Raw 10s monitor logs (build, ephemeral `/tmp/lhcc-hook-exp/`): `monitor.log` (worker),
  `cp-monitor.log` (CP) — machine conditions/annotations, node state, VM/VMI state, LH health,
  controller log tails.
- Controller log lines, worker window: `Registered Longhorn pre-terminate hook` /
  `Triggering Longhorn eviction` / `Deleted bad replicas (stopped-on-gone or on deleting node) to
  force rebuild` / `Node diskStatus stale (manager likely dead) but no replica CRs remain on node —
  treating eviction as complete` / `Early release: eviction drained, volumes degraded-but-safe,
  rebuild blocked only by departing node — releasing hook` / `Removed pre-terminate hook annotation`
- Controller log lines, CP window: `Registered Longhorn pre-terminate hook` (×2) /
  `Longhorn Node CR not found — assuming Longhorn not installed or already cleaned up, releasing
  hook` / `Removed pre-terminate hook annotation`
- Machine events: `LonghornEarlyRelease`, `SuccessfulDrainNode`, `NodeVolumesDetached` (worker);
  `NodeVolumesDetached` (CP)
- Post-hop state verified: 6/6 machines Running (qvvdr→psjmw, rmb85→rmvlx), 6/6 nodes Ready,
  etcd 3 members, 4/4 Longhorn volumes healthy, writers 0 restarts.