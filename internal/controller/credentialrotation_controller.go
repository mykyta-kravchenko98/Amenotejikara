// Package controller contains the CredentialRotation reconciler - the
// piece that watches for a rotated credential landing in the pending
// Secret, applies it to the backend via a rotator.Rotator, and only then
// lets the live Secret (and its consumers) see it.
package controller

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	opsv1alpha1 "github.com/mykyta-kravchenko98/Amenotejikara/api/v1alpha1"
	"github.com/mykyta-kravchenko98/Amenotejikara/internal/rotator"
	"github.com/mykyta-kravchenko98/Amenotejikara/internal/rotator/postgres"
)

// resyncInterval is the safety-net RequeueAfter applied even when a
// reconcile finds nothing to do. Watches are event-driven and normally
// fire within milliseconds of a real change, but a missed event (e.g. the
// controller was down when the pending Secret changed) shouldn't mean
// "stuck out of sync forever" - this bounds how long that can last.
const resyncInterval = 10 * time.Minute

// Reconciler reconciles a CredentialRotation object.
type Reconciler struct {
	client.Client

	// RotatorFactory overrides rotatorFor when set - the seam tests use to
	// inject a fake rotator.Rotator instead of dialing a real backend.
	// Production code (SetupWithManager) leaves this nil, so Reconcile
	// falls back to rotatorFor's normal Target.Type dispatch.
	RotatorFactory func(opsv1alpha1.RotationTarget) (rotator.Rotator, error)
}

// +kubebuilder:rbac:groups=ops.amenotejikara.dev,resources=credentialrotations,verbs=get;list;watch
// +kubebuilder:rbac:groups=ops.amenotejikara.dev,resources=credentialrotations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch

// Reconcile implements the two-Secret consistency model: compare
// PendingSecretRef against LiveSecretRef, and if they differ, apply the
// change to the backend before ever touching LiveSecretRef.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var cr opsv1alpha1.CredentialRotation
	if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
		// NotFound means the CredentialRotation was deleted between the
		// watch event firing and this reconcile running - nothing to do,
		// and importantly not an error (a returned error would just cause
		// a pointless retry against an object that no longer exists).
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	pending, err := r.readCredentials(ctx, cr.Namespace, cr.Spec.PendingSecretRef)
	if err != nil {
		return r.failed(ctx, &cr, fmt.Errorf("reading pending secret: %w", err))
	}

	live, err := r.readCredentials(ctx, cr.Namespace, cr.Spec.LiveSecretRef)
	if err != nil {
		return r.failed(ctx, &cr, fmt.Errorf("reading live secret: %w", err))
	}

	if pending.Password == live.Password {
		// Steady state - the common case between rotations. Nothing
		// rotated just now, so LastRotatedAt must not be touched.
		return r.succeeded(ctx, &cr, false)
	}

	logger.Info("pending credential differs from live, rotating",
		"credentialrotation", cr.Name)

	factory := r.rotatorFor
	if r.RotatorFactory != nil {
		factory = r.RotatorFactory
	}
	rot, err := factory(cr.Spec.Target)
	if err != nil {
		return r.failed(ctx, &cr, err)
	}

	if err := rot.Apply(ctx, live, pending); err != nil {
		// live Secret is untouched, nothing gets rolled - this is exactly
		// the "left in a safe state" guarantee the design relies on.
		return r.failed(ctx, &cr, fmt.Errorf("applying rotation to backend: %w", err))
	}

	if err := r.writeLiveSecret(ctx, cr.Namespace, cr.Spec.LiveSecretRef, pending); err != nil {
		// The backend now expects `pending`, but we failed to persist that
		// into LiveSecretRef (writeLiveSecret already retried transient
		// conflicts internally - this is a harder failure). Left alone,
		// the *next* reconcile would try rot.Apply(live, pending) again
		// using live's now-stale password as "current", which would fail
		// forever: the backend no longer accepts it. Roll the backend
		// back to live's password instead, so the invariant "the backend
		// always accepts whatever's in LiveSecretRef" holds again and the
		// next reconcile can retry the rotation cleanly from scratch.
		if rollbackErr := rot.Apply(ctx, pending, live); rollbackErr != nil {
			// Both the forward write and the compensating rollback
			// failed - this is the one case that genuinely needs a
			// human: the backend and LiveSecretRef may actually disagree
			// now.
			return r.failed(ctx, &cr, fmt.Errorf(
				"backend rotated but failed to update live secret (%v), AND rollback failed (%v) - MANUAL INTERVENTION NEEDED, backend and live secret may be out of sync",
				err, rollbackErr))
		}
		return r.failed(ctx, &cr, fmt.Errorf("failed to update live secret, rolled backend password back safely, will retry: %w", err))
	}

	for _, wl := range cr.Spec.WorkloadRefs {
		if err := r.rollWorkload(ctx, wl); err != nil {
			return r.failed(ctx, &cr, fmt.Errorf("rolling workload %s/%s: %w", wl.Namespace, wl.Name, err))
		}
	}

	logger.Info("rotation applied and workloads rolled", "credentialrotation", cr.Name)
	return r.succeeded(ctx, &cr, true)
}

// readCredentials fetches a Secret and extracts the username/password at
// the keys named in ref.
func (r *Reconciler) readCredentials(ctx context.Context, namespace string, ref opsv1alpha1.SecretKeyRef) (rotator.Credentials, error) {
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Name}, &secret); err != nil {
		return rotator.Credentials{}, fmt.Errorf("get secret %q: %w", ref.Name, err)
	}

	username, ok := secret.Data[ref.UsernameKey]
	if !ok {
		return rotator.Credentials{}, fmt.Errorf("secret %q missing key %q", ref.Name, ref.UsernameKey)
	}
	password, ok := secret.Data[ref.PasswordKey]
	if !ok {
		return rotator.Credentials{}, fmt.Errorf("secret %q missing key %q", ref.Name, ref.PasswordKey)
	}

	return rotator.Credentials{Username: string(username), Password: string(password)}, nil
}

// writeLiveSecret is the only place in the codebase allowed to mutate a
// LiveSecretRef Secret's credential keys - the whole consistency guarantee
// rests on that being true.
//
// Wrapped in retry.RetryOnConflict: the most likely reason this Update
// fails is a resourceVersion conflict (something else touched the Secret
// between our Get and Update - a normal race in the Kubernetes API, not a
// real problem), which RetryOnConflict resolves by re-fetching and
// retrying the whole read-modify-write a bounded number of times. Only if
// that's exhausted (or a non-conflict error occurs) does this return an
// error - at which point Reconcile's caller falls back to the DB-rollback
// compensating action rather than leaving a three-way desync.
func (r *Reconciler) writeLiveSecret(ctx context.Context, namespace string, ref opsv1alpha1.SecretKeyRef, creds rotator.Credentials) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var secret corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Name}, &secret); err != nil {
			return fmt.Errorf("get secret %q: %w", ref.Name, err)
		}

		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data[ref.UsernameKey] = []byte(creds.Username)
		secret.Data[ref.PasswordKey] = []byte(creds.Password)

		return r.Update(ctx, &secret)
	})
	if err != nil {
		return fmt.Errorf("update secret %q: %w", ref.Name, err)
	}
	return nil
}

// rotatorFor dispatches on Target.Type. Adding a new backend means adding
// a case here and a new package under internal/rotator - nothing else in
// the reconciler changes.
func (r *Reconciler) rotatorFor(target opsv1alpha1.RotationTarget) (rotator.Rotator, error) {
	switch target.Type {
	case "postgres":
		if target.Postgres == nil {
			return nil, fmt.Errorf("target.type is %q but target.postgres is unset", target.Type)
		}
		p := target.Postgres
		return postgres.New(p.Host, p.Database, p.Port, p.SSLMode), nil
	default:
		return nil, fmt.Errorf("unsupported target.type %q", target.Type)
	}
}

// rollWorkload restarts a workload so it picks up the now-updated live
// Secret. Only apps/v1 Deployments are supported today - the same
// "explicit unsupported case, no silent partial behavior" approach as
// postgres.Rotator's username-change rejection.
func (r *Reconciler) rollWorkload(ctx context.Context, ref opsv1alpha1.WorkloadRef) error {
	if ref.APIVersion != "apps/v1" || ref.Kind != "Deployment" {
		return fmt.Errorf("unsupported workload ref %s/%s (only apps/v1 Deployment is implemented)", ref.APIVersion, ref.Kind)
	}

	var dep appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, &dep); err != nil {
		return fmt.Errorf("get deployment: %w", err)
	}

	if dep.Spec.Template.Annotations == nil {
		dep.Spec.Template.Annotations = map[string]string{}
	}
	// Same mechanism `kubectl rollout restart` uses under the hood: a pod
	// template annotation change is enough to trigger a rolling update,
	// with no need to touch replica count or image tag.
	dep.Spec.Template.Annotations["amenotejikara.dev/restartedAt"] = time.Now().UTC().Format(time.RFC3339)

	if err := r.Update(ctx, &dep); err != nil {
		return fmt.Errorf("update deployment: %w", err)
	}
	return nil
}

// succeeded records a successful reconcile and schedules the safety-net
// resync. rotated distinguishes "just applied a real rotation" (updates
// LastRotatedAt) from "found pending == live, nothing to do" (leaves
// LastRotatedAt untouched) - both are a successful reconcile, but only one
// of them is an actual rotation event.
func (r *Reconciler) succeeded(ctx context.Context, cr *opsv1alpha1.CredentialRotation, rotated bool) (ctrl.Result, error) {
	cr.Status.Phase = opsv1alpha1.PhaseInSync
	cr.Status.Message = ""
	cr.Status.ObservedGeneration = cr.Generation
	if rotated {
		now := metav1.Now()
		cr.Status.LastRotatedAt = &now
	}
	if err := r.Status().Update(ctx, cr); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}
	return ctrl.Result{RequeueAfter: resyncInterval}, nil
}

// failed records a failed reconcile and returns the error so
// controller-runtime's built-in exponential backoff kicks in - deliberately
// not a custom retry loop, letting the standard rate limiter (starts small,
// caps around 16m by default) avoid hammering a possibly-broken backend.
func (r *Reconciler) failed(ctx context.Context, cr *opsv1alpha1.CredentialRotation, cause error) (ctrl.Result, error) {
	log.FromContext(ctx).Error(cause, "reconcile failed", "credentialrotation", cr.Name)

	cr.Status.Phase = opsv1alpha1.PhaseFailed
	cr.Status.Message = cause.Error()
	if err := r.Status().Update(ctx, cr); err != nil {
		// The status write itself failing is secondary to the original
		// cause - report the original error, not this one, but don't
		// silently drop that the status update also failed.
		log.FromContext(ctx).Error(err, "additionally failed to update status")
	}
	return ctrl.Result{}, cause
}

// SetupWithManager wires the reconciler into the manager: watches
// CredentialRotation directly, plus Secrets indirectly via a mapping
// function (Secrets aren't owned by a CredentialRotation - there's no
// OwnerReference between them - so a plain .Owns() won't do; we have to
// tell controller-runtime explicitly how to go from "this Secret changed"
// to "these CredentialRotations need reconciling").
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&opsv1alpha1.CredentialRotation{}).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.mapSecretToRequests),
		).
		Complete(r)
}

// mapSecretToRequests finds every CredentialRotation in the changed
// Secret's namespace whose PendingSecretRef or LiveSecretRef points at it,
// and returns a reconcile Request for each. This does a List() call per
// Secret event - fine at the scale this project targets; if this ever
// needs to scale to many CredentialRotations per namespace, the standard
// fix is a field index on spec.pendingSecretRef.name via
// mgr.GetFieldIndexer().IndexField, turning this into an indexed lookup
// instead of a full list-and-filter.
func (r *Reconciler) mapSecretToRequests(ctx context.Context, secret client.Object) []ctrl.Request {
	var list opsv1alpha1.CredentialRotationList
	if err := r.List(ctx, &list, client.InNamespace(secret.GetNamespace())); err != nil {
		log.FromContext(ctx).Error(err, "listing CredentialRotations for secret mapping")
		return nil
	}

	var requests []ctrl.Request
	for _, cr := range list.Items {
		if cr.Spec.PendingSecretRef.Name == secret.GetName() || cr.Spec.LiveSecretRef.Name == secret.GetName() {
			requests = append(requests, ctrl.Request{
				NamespacedName: types.NamespacedName{Namespace: cr.Namespace, Name: cr.Name},
			})
		}
	}
	return requests
}
