package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	opsv1alpha1 "github.com/mykyta-kravchenko98/Amenotejikara/api/v1alpha1"
	"github.com/mykyta-kravchenko98/Amenotejikara/internal/rotator"
)

// fakeRotator is a rotator.Rotator test double. It records every Apply call
// and lets tests script success/failure per call, so the controller's
// rotation and rollback paths can be exercised without a real backend.
type fakeRotator struct {
	// applyErrs is consumed in order, one entry per Apply call; a nil entry
	// (or running out of entries) means "succeed".
	applyErrs []error
	calls     []rotator.Credentials // desired argument of each Apply call
}

func (f *fakeRotator) Apply(_ context.Context, _, desired rotator.Credentials) error {
	var err error
	if len(f.applyErrs) > len(f.calls) {
		err = f.applyErrs[len(f.calls)]
	}
	f.calls = append(f.calls, desired)
	return err
}

var _ rotator.Rotator = (*fakeRotator)(nil)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, opsv1alpha1.AddToScheme(scheme))
	return scheme
}

const testNamespace = "shorturl"

func newSecret(name string, data map[string]string) *corev1.Secret {
	byteData := map[string][]byte{}
	for k, v := range data {
		byteData[k] = []byte(v)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Data:       byteData,
	}
}

func newCredentialRotation(name, pendingSecret, liveSecret string, workloadRefs []opsv1alpha1.WorkloadRef) *opsv1alpha1.CredentialRotation {
	return &opsv1alpha1.CredentialRotation{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Generation: 1},
		Spec: opsv1alpha1.CredentialRotationSpec{
			PendingSecretRef: opsv1alpha1.SecretKeyRef{Name: pendingSecret, UsernameKey: "POSTGRES_USER", PasswordKey: "POSTGRES_PASSWORD"},
			LiveSecretRef:    opsv1alpha1.SecretKeyRef{Name: liveSecret, UsernameKey: "POSTGRES_USER", PasswordKey: "POSTGRES_PASSWORD"},
			Target:           opsv1alpha1.RotationTarget{Type: "postgres", Postgres: &opsv1alpha1.PostgresTarget{Host: "postgres", Database: "db"}},
			WorkloadRefs:     workloadRefs,
		},
	}
}

func reconcileRequest(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: name}}
}

func getCR(t *testing.T, c client.Client, name string) opsv1alpha1.CredentialRotation {
	t.Helper()
	var cr opsv1alpha1.CredentialRotation
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: name}, &cr))
	return cr
}

func getSecretPassword(t *testing.T, c client.Client, name string) string {
	t.Helper()
	var secret corev1.Secret
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: name}, &secret))
	return string(secret.Data["POSTGRES_PASSWORD"])
}

func TestReconcile_InSync_NoOp(t *testing.T) {
	cr := newCredentialRotation("shorturl-postgres", "pending", "live", nil)
	pending := newSecret("pending", map[string]string{"POSTGRES_USER": "user", "POSTGRES_PASSWORD": "same-pass"})
	live := newSecret("live", map[string]string{"POSTGRES_USER": "user", "POSTGRES_PASSWORD": "same-pass"})

	fr := &fakeRotator{}
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&opsv1alpha1.CredentialRotation{}).
		WithObjects(cr, pending, live).
		Build()
	r := &Reconciler{Client: c, RotatorFactory: func(opsv1alpha1.RotationTarget) (rotator.Rotator, error) { return fr, nil }}

	res, err := r.Reconcile(context.Background(), reconcileRequest("shorturl-postgres"))
	require.NoError(t, err)
	assert.Equal(t, resyncInterval, res.RequeueAfter)
	assert.Empty(t, fr.calls, "rotator must not be invoked when pending already matches live")

	got := getCR(t, c, "shorturl-postgres")
	assert.Equal(t, opsv1alpha1.PhaseInSync, got.Status.Phase)
	assert.Nil(t, got.Status.LastRotatedAt, "no rotation happened, LastRotatedAt must stay unset")
}

func TestReconcile_RotatesAndUpdatesLiveSecretAndRollsWorkload(t *testing.T) {
	cr := newCredentialRotation("shorturl-postgres", "pending", "live", []opsv1alpha1.WorkloadRef{
		{APIVersion: "apps/v1", Kind: "Deployment", Name: "shorturl", Namespace: testNamespace},
	})
	pending := newSecret("pending", map[string]string{"POSTGRES_USER": "user", "POSTGRES_PASSWORD": "new-pass"})
	live := newSecret("live", map[string]string{"POSTGRES_USER": "user", "POSTGRES_PASSWORD": "old-pass"})
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "shorturl", Namespace: testNamespace},
		Spec:       appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{}},
	}

	fr := &fakeRotator{}
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&opsv1alpha1.CredentialRotation{}).
		WithObjects(cr, pending, live, dep).
		Build()
	r := &Reconciler{Client: c, RotatorFactory: func(opsv1alpha1.RotationTarget) (rotator.Rotator, error) { return fr, nil }}

	_, err := r.Reconcile(context.Background(), reconcileRequest("shorturl-postgres"))
	require.NoError(t, err)

	require.Len(t, fr.calls, 1, "rotator.Apply must be called exactly once for a real rotation")
	assert.Equal(t, "new-pass", fr.calls[0].Password)

	assert.Equal(t, "new-pass", getSecretPassword(t, c, "live"), "live secret must end up with the pending password")

	var gotDep appsv1.Deployment
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "shorturl"}, &gotDep))
	assert.NotEmpty(t, gotDep.Spec.Template.Annotations["amenotejikara.dev/restartedAt"], "workload must be rolled after a successful rotation")

	got := getCR(t, c, "shorturl-postgres")
	assert.Equal(t, opsv1alpha1.PhaseInSync, got.Status.Phase)
	require.NotNil(t, got.Status.LastRotatedAt, "a real rotation must set LastRotatedAt")
}

func TestReconcile_BackendApplyFails_LiveSecretUntouched(t *testing.T) {
	cr := newCredentialRotation("shorturl-postgres", "pending", "live", nil)
	pending := newSecret("pending", map[string]string{"POSTGRES_USER": "user", "POSTGRES_PASSWORD": "new-pass"})
	live := newSecret("live", map[string]string{"POSTGRES_USER": "user", "POSTGRES_PASSWORD": "old-pass"})

	fr := &fakeRotator{applyErrs: []error{errors.New("connection refused")}}
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&opsv1alpha1.CredentialRotation{}).
		WithObjects(cr, pending, live).
		Build()
	r := &Reconciler{Client: c, RotatorFactory: func(opsv1alpha1.RotationTarget) (rotator.Rotator, error) { return fr, nil }}

	_, err := r.Reconcile(context.Background(), reconcileRequest("shorturl-postgres"))
	require.Error(t, err)

	assert.Equal(t, "old-pass", getSecretPassword(t, c, "live"), "a failed backend Apply must leave the live secret alone")

	got := getCR(t, c, "shorturl-postgres")
	assert.Equal(t, opsv1alpha1.PhaseFailed, got.Status.Phase)
	assert.Contains(t, got.Status.Message, "connection refused")
}

// TestReconcile_LiveSecretWriteFails_RollsBackendBack is the safety-critical
// path: the backend accepted the new password, but persisting it into
// LiveSecretRef failed. The controller must compensate by rolling the
// backend's password back to the still-live value, so the invariant "the
// backend always accepts whatever's in LiveSecretRef" holds for the next
// reconcile.
func TestReconcile_LiveSecretWriteFails_RollsBackendBack(t *testing.T) {
	cr := newCredentialRotation("shorturl-postgres", "pending", "live", nil)
	pending := newSecret("pending", map[string]string{"POSTGRES_USER": "user", "POSTGRES_PASSWORD": "new-pass"})
	live := newSecret("live", map[string]string{"POSTGRES_USER": "user", "POSTGRES_PASSWORD": "old-pass"})

	fr := &fakeRotator{}
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&opsv1alpha1.CredentialRotation{}).
		WithObjects(cr, pending, live).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if secret, ok := obj.(*corev1.Secret); ok && secret.Name == "live" {
					return errors.New("simulated conflict: too many retries")
				}
				return c.Update(ctx, obj, opts...)
			},
		}).
		Build()
	r := &Reconciler{Client: c, RotatorFactory: func(opsv1alpha1.RotationTarget) (rotator.Rotator, error) { return fr, nil }}

	_, err := r.Reconcile(context.Background(), reconcileRequest("shorturl-postgres"))
	require.Error(t, err)

	require.Len(t, fr.calls, 2, "forward Apply(live->pending) plus a compensating rollback Apply(pending->live)")
	assert.Equal(t, "new-pass", fr.calls[0].Password, "first call rotates the backend forward to the pending password")
	assert.Equal(t, "old-pass", fr.calls[1].Password, "second call rolls the backend back to the still-live password")

	got := getCR(t, c, "shorturl-postgres")
	assert.Equal(t, opsv1alpha1.PhaseFailed, got.Status.Phase)
	assert.Contains(t, got.Status.Message, "rolled backend password back safely")
}

func TestReconcile_UnsupportedTargetType(t *testing.T) {
	cr := newCredentialRotation("shorturl-postgres", "pending", "live", nil)
	cr.Spec.Target = opsv1alpha1.RotationTarget{Type: "mysql"}
	pending := newSecret("pending", map[string]string{"POSTGRES_USER": "user", "POSTGRES_PASSWORD": "new-pass"})
	live := newSecret("live", map[string]string{"POSTGRES_USER": "user", "POSTGRES_PASSWORD": "old-pass"})

	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&opsv1alpha1.CredentialRotation{}).
		WithObjects(cr, pending, live).
		Build()
	// No RotatorFactory override - exercises the real rotatorFor dispatch.
	r := &Reconciler{Client: c}

	_, err := r.Reconcile(context.Background(), reconcileRequest("shorturl-postgres"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unsupported target.type "mysql"`)

	got := getCR(t, c, "shorturl-postgres")
	assert.Equal(t, opsv1alpha1.PhaseFailed, got.Status.Phase)
}

func TestReconcile_MissingPendingSecret_Failed(t *testing.T) {
	cr := newCredentialRotation("shorturl-postgres", "pending", "live", nil)
	live := newSecret("live", map[string]string{"POSTGRES_USER": "user", "POSTGRES_PASSWORD": "old-pass"})

	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&opsv1alpha1.CredentialRotation{}).
		WithObjects(cr, live).
		Build()
	r := &Reconciler{Client: c}

	_, err := r.Reconcile(context.Background(), reconcileRequest("shorturl-postgres"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading pending secret")

	got := getCR(t, c, "shorturl-postgres")
	assert.Equal(t, opsv1alpha1.PhaseFailed, got.Status.Phase)
}

func TestReconcile_MissingUsernameKey_Failed(t *testing.T) {
	cr := newCredentialRotation("shorturl-postgres", "pending", "live", nil)
	pending := newSecret("pending", map[string]string{"POSTGRES_PASSWORD": "new-pass"}) // no username key
	live := newSecret("live", map[string]string{"POSTGRES_USER": "user", "POSTGRES_PASSWORD": "old-pass"})

	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&opsv1alpha1.CredentialRotation{}).
		WithObjects(cr, pending, live).
		Build()
	r := &Reconciler{Client: c}

	_, err := r.Reconcile(context.Background(), reconcileRequest("shorturl-postgres"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `missing key "POSTGRES_USER"`)
}

func TestReconcile_CredentialRotationDeleted_NoError(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&opsv1alpha1.CredentialRotation{}).
		Build()
	r := &Reconciler{Client: c}

	res, err := r.Reconcile(context.Background(), reconcileRequest("does-not-exist"))
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res)
}

func TestReconcile_UnsupportedWorkloadKind_Failed(t *testing.T) {
	cr := newCredentialRotation("shorturl-postgres", "pending", "live", []opsv1alpha1.WorkloadRef{
		{APIVersion: "apps/v1", Kind: "StatefulSet", Name: "postgres", Namespace: testNamespace},
	})
	pending := newSecret("pending", map[string]string{"POSTGRES_USER": "user", "POSTGRES_PASSWORD": "new-pass"})
	live := newSecret("live", map[string]string{"POSTGRES_USER": "user", "POSTGRES_PASSWORD": "old-pass"})

	fr := &fakeRotator{}
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&opsv1alpha1.CredentialRotation{}).
		WithObjects(cr, pending, live).
		Build()
	r := &Reconciler{Client: c, RotatorFactory: func(opsv1alpha1.RotationTarget) (rotator.Rotator, error) { return fr, nil }}

	_, err := r.Reconcile(context.Background(), reconcileRequest("shorturl-postgres"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only apps/v1 Deployment is implemented")

	assert.Equal(t, "new-pass", getSecretPassword(t, c, "live"))
}

func TestRotatorFor(t *testing.T) {
	r := &Reconciler{}

	t.Run("postgres", func(t *testing.T) {
		rot, err := r.rotatorFor(opsv1alpha1.RotationTarget{
			Type:     "postgres",
			Postgres: &opsv1alpha1.PostgresTarget{Host: "postgres", Database: "db"},
		})
		require.NoError(t, err)
		assert.NotNil(t, rot)
	})

	t.Run("postgres without config", func(t *testing.T) {
		_, err := r.rotatorFor(opsv1alpha1.RotationTarget{Type: "postgres"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "target.postgres is unset")
	})

	t.Run("unsupported type", func(t *testing.T) {
		_, err := r.rotatorFor(opsv1alpha1.RotationTarget{Type: "mongodb"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), `unsupported target.type "mongodb"`)
	})
}

func TestMapSecretToRequests(t *testing.T) {
	crA := newCredentialRotation("cr-a", "pending-a", "live-a", nil)
	crB := newCredentialRotation("cr-b", "pending-b", "live-b", nil)
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(crA, crB).
		Build()
	r := &Reconciler{Client: c}

	t.Run("matches pending ref", func(t *testing.T) {
		secret := newSecret("pending-a", nil)
		reqs := r.mapSecretToRequests(context.Background(), secret)
		require.Len(t, reqs, 1)
		assert.Equal(t, "cr-a", reqs[0].Name)
	})

	t.Run("matches live ref", func(t *testing.T) {
		secret := newSecret("live-b", nil)
		reqs := r.mapSecretToRequests(context.Background(), secret)
		require.Len(t, reqs, 1)
		assert.Equal(t, "cr-b", reqs[0].Name)
	})

	t.Run("unrelated secret matches nothing", func(t *testing.T) {
		secret := newSecret("some-other-secret", nil)
		reqs := r.mapSecretToRequests(context.Background(), secret)
		assert.Empty(t, reqs)
	})
}
