package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SecretKeyRef points at a Kubernetes Secret (in the same namespace as the
// owning CredentialRotation) and the keys within its data holding a
// username/password pair.
type SecretKeyRef struct {
	// Name of the Secret.
	Name string `json:"name"`
	// UsernameKey is the key in the Secret's data holding the username.
	UsernameKey string `json:"usernameKey"`
	// PasswordKey is the key in the Secret's data holding the password.
	PasswordKey string `json:"passwordKey"`
}

// PostgresTarget holds what's needed to connect to a Postgres database
// whose user password should track LiveSecretRef. It deliberately does not
// include credentials - those come from LiveSecretRef/PendingSecretRef.
type PostgresTarget struct {
	Host     string `json:"host"`
	Database string `json:"database"`
	// +optional
	// +kubebuilder:default="5432"
	Port string `json:"port,omitempty"`
	// +optional
	// +kubebuilder:default="disable"
	SSLMode string `json:"sslMode,omitempty"`
}

// RotationTarget is a discriminated union over backend types: Type selects
// which Rotator implementation handles this CredentialRotation, and which
// of the type-specific fields below is populated. Postgres is the only
// implementation today; adding a backend means adding a field here (e.g.
// MySQL *MySQLTarget) and a case in the Rotator dispatch, nothing else.
type RotationTarget struct {
	// +kubebuilder:validation:Enum=postgres
	Type string `json:"type"`

	// +optional
	Postgres *PostgresTarget `json:"postgres,omitempty"`
}

// WorkloadRef identifies a workload to roll once a rotation has been
// applied to the backend, so it picks up the now-consistent LiveSecretRef.
type WorkloadRef struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
}

// CredentialRotationSpec is the user-authored, desired state.
type CredentialRotationSpec struct {
	// PendingSecretRef is the Secret something else (typically an
	// ExternalSecret) syncs a freshly-rotated credential into. The
	// controller only ever reads this Secret.
	PendingSecretRef SecretKeyRef `json:"pendingSecretRef"`

	// LiveSecretRef is the Secret consumers actually mount. The controller
	// is the sole writer of this Secret's credential keys - it only
	// updates them after confirming the backend accepted the new value.
	LiveSecretRef SecretKeyRef `json:"liveSecretRef"`

	// Target describes the backend to keep in sync with LiveSecretRef.
	Target RotationTarget `json:"target"`

	// WorkloadRefs lists workloads to roll after a successful rotation.
	// +optional
	WorkloadRefs []WorkloadRef `json:"workloadRefs,omitempty"`
}

// RotationPhase summarizes where a CredentialRotation currently stands.
type RotationPhase string

const (
	// PhaseInSync means LiveSecretRef already matches PendingSecretRef -
	// the common steady state between rotations.
	PhaseInSync RotationPhase = "InSync"
	// PhaseRotationPending means a difference was observed and the
	// controller is (or is about to start) applying it to the backend.
	PhaseRotationPending RotationPhase = "RotationPending"
	// PhaseFailed means the last rotation attempt failed - LiveSecretRef
	// was left untouched and nothing was rolled. Needs investigation.
	PhaseFailed RotationPhase = "Failed"
)

// CredentialRotationStatus is the controller-owned observed state.
type CredentialRotationStatus struct {
	// +optional
	Phase RotationPhase `json:"phase,omitempty"`
	// +optional
	LastRotatedAt *metav1.Time `json:"lastRotatedAt,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// ObservedGeneration is the .metadata.generation the controller last
	// finished reconciling - lets `kubectl get` / callers tell "reflects
	// the latest spec edit" apart from "stale, reconcile still pending".
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Last Rotated",type=date,JSONPath=`.status.lastRotatedAt`

// CredentialRotation keeps a stateful backend's credential in sync with a
// value rotated into PendingSecretRef, then rolls WorkloadRefs. See the
// project README for the full design and the two-Secret consistency model.
type CredentialRotation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CredentialRotationSpec   `json:"spec,omitempty"`
	Status CredentialRotationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// CredentialRotationList is a list of CredentialRotation - client-go/
// controller-runtime require a List type alongside every Kind to support
// `kubectl get credentialrotations` and List() calls.
type CredentialRotationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CredentialRotation `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CredentialRotation{}, &CredentialRotationList{})
}
