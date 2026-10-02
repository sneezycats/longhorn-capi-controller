# Least-priv workload credential — setup + validation cycle (2026-10-02)

Round-5 follow-up. Goal: prove the controller functions under a least-privilege workload
credential instead of cluster-owner, and shake out missing verbs. Companion to
`docs/pre-drain-hook-experiment-2026-10-02.md` (same day, same roll2 environment).

## Headline results

1. **The shipped `config/rbac/workload_role.yaml` is complete and validated.** A full machine
   deletion cycle ran with the controller operating on a least-priv ServiceAccount credential:
   **zero forbidden errors** across every operation — LH node eviction, replica deletes, CRIT-1a
   last-replica refusal, CRIT-1b relocation (`numberOfReplicas` 1→2 patch + restore), cordon,
   PVC reads, and the CAPI drain itself (eviction API) all succeeded.
2. **Read-only for non-Longhorn clusters is covered by the same role.** The controller's guard is
   a `nodes.longhorn.io` GET (verified no-op on CP machines: "Longhorn Node CR not found —
   assuming Longhorn not installed"); the GET is in the role, and RBAC grants on absent CRDs are
   harmless. No additional permission is needed for GKE/AKS/Portworx clusters.
3. **BLOCKER — the credential must NOT live in the shared `<cluster>-kubeconfig` Secret.**
   Swapping the secret's `value` tripped a deterministic nil-pointer panic in rancher 2.14.3
   (all 3 rancher replicas CrashLoopBackOff). Root cause pinned; recovery trivial; the design
   consequence is a required controller change (dedicated secret). Details below.

## What was set up (in the workload cluster, lhcc-roll2)

- `ClusterRole longhorn-capi-eviction-workload` — applied from `config/rbac/workload_role.yaml`
  (was NOT present in roll2; roll1 is gone). Left applied at the shipped spec.
- `ServiceAccount kube-system/lhcc-eviction-agent` + never-expiring token Secret
  (`kubernetes.io/service-account-token`). kube-system chosen so the same identity pattern exists
  on every guest cluster (LH or not).
- `ClusterRoleBinding longhorn-capi-eviction-workload` — subject repointed from the shipped
  `REPLACE_WITH_KUBECONFIG_IDENTITY` placeholder to the SA. This is the end state the repo file
  documents ("bind whatever identity that kubeconfig carries").
- Test kubeconfig (build `~/lhcc-leastpriv/`): direct apiserver `https://192.168.1.149:6443`,
  cluster CA from the downstream `kube-root-ca.crt` ConfigMap, SA token inline.
- The mgmt Secret `fleet-default/lhcc-roll2-kubeconfig` `value` was swapped to the SA kubeconfig
  for the validation cycle (original backed up at build `~/lhcc-leastpriv/lhcc-roll2-kubeconfig-admin-backup.yaml`)
  and **reverted** after the cycle (see blocker). The Secret's `token` key was never touched.

## Permission validation (static)

- SSRR dump matches the role exactly; targeted `auth can-i` all yes for every controller call site.
- Quirk worth recording: `kubectl auth can-i create pods/eviction` reports **no** even when the
  rule is bound — a kubectl subresource-parsing artifact. The raw SelfSubjectAccessReview
  (`resource: pods, subresource: eviction`) returns `allowed: true`, and a live eviction POST
  (`POST /api/v1/namespaces/<ns>/pods/<name>/eviction`, the exact path client-go's drainer uses)
  returns **201 Success**. Verify eviction with a raw SAR or a live POST, never `can-i`.
- Negative checks confirm least-priv: no Secrets, no CRD list, no pod create.

## Validation cycle (least-priv, worker lhcc-roll2-workers-crpdq-xfl4x)

t0 = 14:55:46Z. The fullest path: this worker held the sole replica of the single-replica volume
(CRIT-1b relocation), 3 replicas of the 3-replica volumes, and 2 volume engines.

| t | event |
|---|---|
| +1s | lhcc: registers pre-terminate hook; triggers LH eviction; deletes the 3 bad replicas; **CRIT-1a refuses the last live replica**; **CRIT-1b relocation: raises want 1→2** (event `LonghornLastReplicaRelocation`) |
| +16–31s | rebuild lands; old sole replica deleted; volumes degraded-but-safe |
| +43s | early release; **want restored 2→1**; hook released (event `LonghornEarlyRelease`) |
| +53s | **`SuccessfulDrainNode`** — the CAPI machine controller drained using the SAME least-priv credential (eviction API), + `NodeVolumesDetached` |
| +18m | replacement worker `77gf5` Ready; LH rebuilt 3/3/3/1; **4/4 volumes healthy** |

- lhcc controller log: **0 forbidden, 0 errors** for the whole window.
- Writer continuity: `lhcc-writer-rep1` kept appending through the entire event
  (counter seq continued, `created-at` unchanged, probe md5s intact), 0 restarts.

## The rancher crash (blocker, root-caused)

Swapping the Secret's `value` → within ~1 minute all three rancher replicas entered
**CrashLoopBackOff** (deterministic: every reconcile replay panicked). Consequences while rancher
was down: agent tunnels dead (rancher k8s-proxy → `ServiceUnavailable`), the HarvesterMachine→VM
teardown stalled (`WaitingForInfrastructureDeletion`), machine `NodeHealthy` conditions stuck at
`ConnectionDown`. The workload cluster itself stayed healthy throughout (direct API fine, LH fine,
writers fine) — all alarming readings through the rancher proxy were proxy-failure artifacts.

Pinned against `rancher/rancher v2.14.3`, `pkg/provisioningv2/kubeconfig/manager.go:251`:

```go
splitServer := strings.Split(kc.Clusters["cluster"].Server, "/k8s/clusters/")
```

`kc.Clusters["cluster"]` indexes the kubeconfig's cluster entry **by the literal name
`cluster`**; any other name → nil map entry → `.Server` → SIGSEGV. Additionally, the surrounding
logic makes the Secret rancher-owned by design: when `kubeConfigValid` returns not-valid — which
includes any server URL that is not a rancher `/k8s/clusters/<id>` proxy URL — rancher
**regenerates and overwrites** the Secret's kubeconfig.

**Conclusion:** a least-priv credential cannot be delivered through the shared
`<cluster>-kubeconfig` Secret — it is rancher's own managed credential (the drainer and the
kubeconfig manager both consume it), it gets regenerated, and malformed shapes crash rancher
2.14.3 (the missing nil check is an upstream bug worth reporting).

**Required controller change:** read a dedicated per-cluster kubeconfig Secret (e.g.
`<cluster>-lhcc-kubeconfig` or a `--workload-kubeconfig-secret-suffix` flag). The identity +
binding built here stay in place and are inert until that ships; the drainer keeps using
rancher's own credential.

## Recovery runbook (for the record)

The swap incident was recovered in ~20 min:

1. `kubectl -n fleet-default patch secret lhcc-roll2-kubeconfig --type=merge -p '{"data":{"value":"<original b64>"}}'`
   (original preserved at build `~/lhcc-leastpriv/lhcc-roll2-kubeconfig-admin-backup.yaml`).
2. `kubectl -n cattle-system delete pod -l app=rancher` (skip the crash backoff timer).
3. Rancher re-conciles, tunnels re-establish, the stalled hop completes normally.

Note: while rancher is CrashLooping, `kubectl` against the mgmt API keeps working — only
rancher-owned paths (proxy, provisioning controllers, UI) fail. Don't misread proxy 503s as
cluster state.

## Final state (verified 15:30Z)

- 6/6 machines Running (xfl4x → 77gf5), 6/6 nodes Ready, etcd 3/3
- 4/4 Longhorn volumes healthy; ready replicas 77gf5:3 psjmw:4 wpr49:4 (one excess trimming)
- Writers: 4/4 Running, 0 restarts, counters/probes intact
- ClusterRole + binding + SA + token in place (inert); mgmt Secret reverted to rancher-managed
  admin kubeconfig; controller running v0.10.0 unchanged
- Net rancher restarts: 3 pods bounced once; no persistent damage

## Open items

1. Controller change: dedicated workload kubeconfig Secret (naming convention + docs).
2. Upstream report: rancher 2.14.3 `kubeConfigValid` nil dereference on non-`cluster`-named
   kubeconfig entries.
3. Re-run a cycle with the controller actually consuming the least-priv credential once the
   dedicated secret lands (the verb surface is already proven; this validates the plumbing).
