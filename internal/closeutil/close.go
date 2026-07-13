// Package closeutil gives deferred Close() calls somewhere to put the
// error instead of silently discarding it (what a bare `defer f.Close()`
// does, and what errcheck flags).
package closeutil

import (
	"context"
	"io"

	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Close calls c.Close() and logs a failure via the logr.Logger embedded in
// ctx - the same per-reconcile logger controller-runtime injects into
// every Reconcile call - rather than a raw stdout/stderr print. Any
// WithValues() attached higher up the call chain (e.g. the
// CredentialRotation's name/namespace) carries through automatically.
//
//	defer closeutil.Close(ctx, db)
func Close(ctx context.Context, c io.Closer) {
	if err := c.Close(); err != nil {
		log.FromContext(ctx).Error(err, "failed to close resource")
	}
}
