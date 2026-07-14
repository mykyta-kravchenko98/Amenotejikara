# Adopting Amenotejikara for an existing ExternalSecret-owned Secret

If a Secret your app already mounts is currently created and owned by an
`ExternalSecret` (the common case: `target.creationPolicy: Owner`, the
default), you can't just point a new `CredentialRotation` at it and delete
the old `ExternalSecret` in one step. Per [ESO's own
docs](https://external-secrets.io/latest/api/externalsecret/#example),
`Owner` sets `.metadata.ownerReferences` on the created Secret, and
**deleting the `ExternalSecret` cascades into deleting the Secret** -
which happens to be the one your running workload depends on.

This guide is a generic, three-phase migration that avoids that risk
entirely. It's written against a Postgres example, but nothing here is
Postgres-specific - swap in whatever backend/`Rotator` applies.

Throughout, `<pending>` and `<live>` refer to the Secret names you choose;
`<live>` is the one your workload already mounts today.

## Phase 1 - add the pending ExternalSecret (safe, additive)

Add a **new**, independent `ExternalSecret` targeting a **new** Secret
name, `<pending>`, pulling from the same remote credential `<live>`
currently gets its data from. This doesn't touch the existing
`ExternalSecret` or `<live>` at all - purely additive.

Verify after deploy:
```bash
kubectl -n <namespace> get secret <pending> -o jsonpath='{.data.PASSWORD}' | base64 -d
kubectl -n <namespace> get secret <live> -o jsonpath='{.data.PASSWORD}' | base64 -d
```
Both should match (same source, nothing's rotated yet).

## Phase 2 - defuse the old ExternalSecret's ownership

Edit the *existing* `ExternalSecret` (the one that owns `<live>`):
change `target.creationPolicy` from `Owner` to `Orphan`, still targeting
`<live>`, still the same data. Per ESO's docs, `Orphan` "creates the
Secret but does not set `.metadata.ownerReferences`" - on the next
reconcile it drops the ownerReference from the *existing* Secret without
touching its data.

Verify after deploy:
```bash
kubectl -n <namespace> get secret <live> -o jsonpath='{.metadata.ownerReferences}'
```
Should print nothing/empty - confirms it's safe to delete the
`ExternalSecret` next without cascading.

### A trap that isn't obvious from the ESO docs alone: label collisions with GitOps pruning

If you're on ArgoCD (or another GitOps tool) with `prune: true` and
label-based resource tracking, and the `ExternalSecret`'s own
`metadata.labels` include your Application's tracking label (commonly
`app.kubernetes.io/instance`), **ESO copies those labels onto the Secret
it creates by default**. Since that Secret was never actually part of
your rendered manifest set (ESO created it, not Helm/Kustomize/raw
apply), ArgoCD can mistake it for an orphaned member of the Application
on a routine sync and prune it - deleting a Secret nothing in the
ExternalSecret or CredentialRotation flow told it to delete.

This bit us during Amenotejikara's own adoption for
[ShortUrl](https://github.com/mykyta-kravchenko98/shorturl-gitops) - see
that repo's git history around `postgres-credentials` going briefly
missing mid-cutover.

The fix: set `target.template.metadata.labels: {}` (and `annotations: {}`
if applicable) on **both** ExternalSecrets involved (`<pending>` and the
old `<live>`-owning one). Per ESO's docs, setting `target.template`
replaces the default label/annotation copy-through instead of adding to
it, so an empty map disables it entirely:

```yaml
spec:
  target:
    name: <live>
    creationPolicy: Orphan
    template:
      metadata:
        labels: {}
        annotations: {}
```

Do this **before** Phase 3, not after - the failure mode is silent until
the next prune-triggering sync.

## Phase 3 - retire the old ExternalSecret

Only after Phase 2 (including the label fix above) is confirmed: delete
the old `ExternalSecret` from git, let your GitOps tool prune the
`ExternalSecret` object. `<live>` stays exactly as it was - no owner, no
longer touched by ESO. From this point on, only Amenotejikara's
`CredentialRotation` reconciler writes to it.

Verify:
```bash
kubectl -n <namespace> get externalsecret <old-name>
# should be NotFound
kubectl -n <namespace> get secret <live>
# should still exist, same data as before
```

## Phase 4 - create the CredentialRotation

Only now, with `<live>` unowned and `<pending>` established, create the
`CredentialRotation` CR pointing `pendingSecretRef` at `<pending>` and
`liveSecretRef` at `<live>` (see the main README's [Example](../README.md#example)).
Since `<pending>` and `<live>` should already hold matching credentials
at this point (nothing's rotated during the cutover itself), the first
reconcile should land in `InSync` with no rotation triggered - confirm
that before considering the migration done:

```bash
kubectl -n <namespace> get credentialrotation <name>
# PHASE should read InSync, with no LAST ROTATED timestamp yet
```

## Not covered here

Automating *when* a rotation happens (ESO's `Generator` (`kind:
Password`) + `PushSecret` "Rotate Secrets" pattern, or any other trigger
on the source-of-truth side) is orthogonal to this cutover and covered in
the main [README](../README.md#non-goals) - Amenotejikara is purely
reactive to whatever lands in `<pending>`.
