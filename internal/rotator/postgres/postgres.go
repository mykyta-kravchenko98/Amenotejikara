// Package postgres implements rotator.Rotator for a Postgres user's
// password via ALTER USER.
package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"

	"github.com/lib/pq"

	"github.com/mykyta-kravchenko98/Amenotejikara/internal/closeutil"
	"github.com/mykyta-kravchenko98/Amenotejikara/internal/rotator"
)

// Rotator connects to a single Postgres database and applies password
// changes via ALTER USER. It holds no credentials itself - those are
// passed into Apply per-call, straight from the CredentialRotation's
// Secrets.
type Rotator struct {
	Host     string
	Port     string
	Database string
	SSLMode  string
}

// Compile-time check that *Rotator satisfies rotator.Rotator. Go has no
// `implements` keyword - interface satisfaction is purely structural (any
// type with matching methods qualifies), so without this line a signature
// mismatch (wrong param order, wrong type) would go unnoticed until
// something elsewhere actually tries to use *Rotator as a rotator.Rotator.
// Assigning a nil pointer to a discarded variable of the interface type
// costs nothing at runtime and fails to compile immediately if the method
// set doesn't match.
var _ rotator.Rotator = (*Rotator)(nil)

// New builds a Rotator, filling in the same defaults the CRD schema
// declares (kubebuilder:default on PostgresTarget) in case a caller
// constructs one directly rather than from a decoded CR.
func New(host, database string, port, sslMode string) *Rotator {
	if port == "" {
		port = "5432"
	}
	if sslMode == "" {
		sslMode = "disable"
	}
	return &Rotator{Host: host, Port: port, Database: database, SSLMode: sslMode}
}

// Apply authenticates as current.Username using current.Password, then
// changes that same user's password to desired.Password.
//
// Username rotation is deliberately out of scope: renaming a Postgres role
// touches ownership and grants across every object it owns, which is a
// meaningfully riskier operation than a password change and shouldn't
// happen implicitly as a side effect of credential rotation.
func (r *Rotator) Apply(ctx context.Context, current, desired rotator.Credentials) error {
	if current.Username != desired.Username {
		return fmt.Errorf("postgres rotator: username change not supported (%q -> %q); only password rotation is implemented",
			current.Username, desired.Username)
	}

	dsn := (&url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(current.Username, current.Password),
		Host:   fmt.Sprintf("%s:%s", r.Host, r.Port),
		Path:   "/" + r.Database,
		RawQuery: url.Values{
			"sslmode": {r.SSLMode},
		}.Encode(),
	}).String()

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return fmt.Errorf("open connection: %w", err)
	}
	defer closeutil.Close(ctx, db)

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping with current credentials: %w", err)
	}

	// ALTER USER is DDL - Postgres does not accept a $1-style bind
	// parameter for the password literal here, so it must be interpolated
	// into the statement text. pq.QuoteIdentifier/pq.QuoteLiteral are
	// lib/pq's own escaping helpers for exactly this situation (they
	// double up embedded quote characters and wrap the value), not a
	// custom escaping scheme.
	stmt := fmt.Sprintf("ALTER USER %s WITH PASSWORD %s",
		pq.QuoteIdentifier(current.Username), pq.QuoteLiteral(desired.Password))
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("alter user: %w", err)
	}

	return nil
}
