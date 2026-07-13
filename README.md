# Amenotejikara

A small Kubernetes operator that closes the one gap [External Secrets
Operator](https://external-secrets.io) deliberately leaves open: ESO can
generate a new credential and push it to any supported secret store, but it
has no idea that a *stateful backend* (a database, a message broker, ...)
also needs to be told about that new credential before anyone can actually
use it.

Amenotejikara watches for a rotated credential landing in a Kubernetes
`Secret`, applies it to the real backend, and only then lets consumers see
it - so there is never a window where a Pod restarts, picks up a "new"
password from its env, and fails to authenticate because the database
doesn't know about it yet.

## Why this exists

The generic secret-rotation story with ESO looks like this:

1. A `Generator` (`kind: Password`) produces a new random value on a
   schedule.
2. A `PushSecret` writes that value out to the external store (AWS Secrets
   Manager, Vault, GCP Secret Manager, ...) - this becomes the source of
   truth.
3. An `ExternalSecret` pulls it back down into a Kubernetes `Secret`.

That's the entire rotation lifecycle ESO understands, and it's genuinely
provider-agnostic. What's missing is step 4: something has to actually run
`ALTER USER ... WITH PASSWORD ...` (or the equivalent for whatever backend
is in play) against the live system, *before* any application is allowed
to pick up the new value - otherwise a Pod that restarts independently
(crash loop, OOM, node drain, HPA scale-out) between steps 3 and 4 grabs
the new password out of the Secret and fails to connect, because the
database is still expecting the old one.

Amenotejikara is that step 4, and nothing else. It never talks to AWS,
GCP, Vault, or any cloud API directly - that responsibility stays fully
with ESO. It only ever watches and writes plain Kubernetes `Secret`
objects, which is what makes it reusable across any project regardless of
which secret store or cloud is behind it.

## How it works

Two `Secret`s per credential, not one:

- **pending** - whatever `ExternalSecret` syncs down from the store. This
  reflects the *desired* state; Amenotejikara only ever reads it.
- **live** - what applications actually mount via `envFrom`/`volumeMounts`.
  Amenotejikara is the *only* writer to this Secret.

On every reconcile (triggered by a watch event on the pending Secret, plus
a periodic safety-net resync):

1. Compare `pending` against `live`. If they match, there's nothing to do.
2. If they differ, connect to the target backend using the credentials
   currently in `live` (still valid - the backend hasn't been touched yet)
   and apply the new credential via a backend-specific `Rotator`.
3. Only on success: copy `pending` into `live`.
4. Roll the workloads listed in `spec.workloadRefs` (a generic list of
   `{apiVersion, kind, name, namespace}` - not tied to any particular app)
   so they pick up the now-consistent `live` Secret.
5. On failure at step 2, `live` is left untouched, nothing gets rolled, and
   the `CredentialRotation` status reports the failure instead of retrying
   in a hot loop.

Because `live` is only ever mutated *after* the backend confirms the
change, any independent Pod restart at any point in time is safe by
construction - there's no timing window to race.

## Example

```yaml
apiVersion: ops.amenotejikara.dev/v1alpha1
kind: CredentialRotation
metadata:
  name: shorturl-postgres
  namespace: shorturl
spec:
  pendingSecretRef:
    name: postgres-credentials-pending
    usernameKey: POSTGRES_USER
    passwordKey: POSTGRES_PASSWORD
  liveSecretRef:
    name: postgres-credentials
    usernameKey: POSTGRES_USER
    passwordKey: POSTGRES_PASSWORD
  target:
    type: postgres
    postgres:
      host: postgres.shorturl.svc.cluster.local
      port: "5432"
      database: ShortURLDataDB
      sslMode: disable
  workloadRefs:
    - apiVersion: apps/v1
      kind: Deployment
      name: shorturl
      namespace: shorturl
status:
  phase: InSync
  lastRotatedAt: "2026-07-13T00:00:00Z"
  observedGeneration: 3
```

## Extending to other backends

`target.type` is a discriminator over a small `Rotator` interface:

```go
type Rotator interface {
    Apply(ctx context.Context, current, desired Credentials) error
}
```

Postgres (`ALTER USER`) is the first implementation. Adding a new backend
means implementing this interface and registering it under a new
`target.type` value - it does not touch the reconcile loop, the two-Secret
consistency model, or anything ESO-facing.

## Non-goals

- Generating credentials. That's ESO's `Generator`.
- Talking to any cloud secret store. That's ESO's `SecretStore` /
  `PushSecret` / `ExternalSecret`.
- Deciding *when* to rotate. Amenotejikara is purely reactive to a change
  already landing in the pending Secret (plus a periodic resync as a
  safety net) - the schedule lives on the `PushSecret`'s
  `refreshInterval`.

## Status

Early scaffolding. First target backend is Postgres, first consumer is
[ShortUrl](https://github.com/mykyta-kravchenko98/shorturl-gitops).

## Name

Amenotejikara is Sasuke's instant body-swap technique in Naruto (via the
Rinnegan) - seemed fitting for something whose whole job is swapping out
credentials underneath consumers without them noticing.
