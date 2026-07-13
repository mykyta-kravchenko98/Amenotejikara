// +kubebuilder:object:generate=true
// +groupName=ops.amenotejikara.dev
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group/version this package's types belong to -
	// what shows up as "apiVersion: ops.amenotejikara.dev/v1alpha1" on the
	// CredentialRotation YAML.
	GroupVersion = schema.GroupVersion{Group: "ops.amenotejikara.dev", Version: "v1alpha1"}

	// SchemeBuilder accumulates the Go types in this package so they can be
	// registered into a controller-runtime manager's runtime.Scheme in one
	// call (see AddToScheme below, and its use in cmd/manager/main.go).
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme registers this package's types (CredentialRotation,
	// CredentialRotationList) into a *runtime.Scheme. The manager needs
	// this to know how to encode/decode our CRD alongside the built-in
	// types (Secret, Deployment, ...) it already knows about.
	AddToScheme = SchemeBuilder.AddToScheme
)
