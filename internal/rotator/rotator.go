// Package rotator defines the pluggable contract between the controller's
// reconcile loop and backend-specific credential actuation. Adding support
// for a new kind of backend (MySQL, RabbitMQ, ...) means implementing
// Rotator in a new subpackage - the reconciler and the CRD's two-Secret
// consistency model never need to change.
package rotator

import "context"

// Credentials is a plain username/password pair - deliberately not tied to
// any particular Secret shape or backend.
type Credentials struct {
	Username string
	Password string
}

// Rotator applies a credential change to a live backend.
//
// Implementations MUST authenticate using `current` (still valid - the
// backend hasn't been touched yet) and, on success, leave the backend
// requiring `desired` for future authentication. Apply must not return nil
// unless the backend has durably accepted the change - the caller treats a
// nil error as the signal that it's now safe to overwrite the live Secret
// and roll consumers.
type Rotator interface {
	Apply(ctx context.Context, current, desired Credentials) error
}
