package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mykyta-kravchenko98/Amenotejikara/internal/rotator"
	"github.com/mykyta-kravchenko98/Amenotejikara/internal/rotator/postgres"
)

// This is an integration test - it needs a real Postgres reachable at the
// env vars below (see .github/workflows/go-build.yml's `services:
// postgres` block for the CI setup). It's deliberately not skipped when
// those env vars are unset - it'll just fail with a connection error,
// which is the same signal "you don't have Postgres running locally" as
// any other integration test in this codebase (see ShortUrl's own test
// suite for the same convention).
func testConfig() (host, port, database, user, password string) {
	return envOr("PG_TEST_HOST", "localhost"),
		envOr("PG_TEST_PORT", "5432"),
		envOr("PG_TEST_DB", "rotator_test"),
		envOr("PG_TEST_USER", "rotator_test"),
		envOr("PG_TEST_PASSWORD", "initial-test-password")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func connect(t *testing.T, host, port, database, username, password string) *sql.DB {
	t.Helper()

	dsn := (&url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(username, password),
		Host:   fmt.Sprintf("%s:%s", host, port),
		Path:   "/" + database,
		RawQuery: url.Values{
			"sslmode": {"disable"},
		}.Encode(),
	}).String()

	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// resetPassword restores the role's password to knownPassword via a
// superuser-free path (connecting as the role itself) so tests don't
// interfere with each other's state across runs. Called via t.Cleanup, so
// it also runs when the test being cleaned up failed midway through
// changing the password.
func resetPassword(t *testing.T, host, port, database, username, currentGuess, resetTo string) {
	t.Helper()
	r := postgres.New(host, database, port, "disable")
	// Best-effort: if currentGuess is already wrong (e.g. a prior test in
	// this same run already reset it), this is a no-op failure we can
	// ignore - the next test's own setup is responsible for getting the
	// password back to a known value before it relies on it.
	_ = r.Apply(context.Background(), rotator.Credentials{Username: username, Password: currentGuess}, rotator.Credentials{Username: username, Password: resetTo})
}

func TestApply_ChangesPassword(t *testing.T) {
	host, port, database, user, initialPassword := testConfig()
	const newPassword = "rotated-test-password-1"

	t.Cleanup(func() { resetPassword(t, host, port, database, user, newPassword, initialPassword) })

	r := postgres.New(host, database, port, "disable")

	err := r.Apply(context.Background(),
		rotator.Credentials{Username: user, Password: initialPassword},
		rotator.Credentials{Username: user, Password: newPassword},
	)
	require.NoError(t, err)

	// The new password must work now...
	dbNew := connect(t, host, port, database, user, newPassword)
	assert.NoError(t, dbNew.PingContext(context.Background()))

	// ...and the old one must no longer work - if it still did, Apply
	// didn't actually change anything backend-side.
	dbOld := connect(t, host, port, database, user, initialPassword)
	assert.Error(t, dbOld.PingContext(context.Background()))
}

func TestApply_WrongCurrentPassword(t *testing.T) {
	host, port, database, user, initialPassword := testConfig()

	r := postgres.New(host, database, port, "disable")

	err := r.Apply(context.Background(),
		rotator.Credentials{Username: user, Password: "definitely-not-the-real-password"},
		rotator.Credentials{Username: user, Password: "irrelevant"},
	)
	require.Error(t, err)

	// And the real password must still work - a failed Apply must never
	// leave the backend in a changed state.
	db := connect(t, host, port, database, user, initialPassword)
	assert.NoError(t, db.PingContext(context.Background()))
}

func TestApply_UsernameChangeRejected(t *testing.T) {
	host, port, database, user, initialPassword := testConfig()

	r := postgres.New(host, database, port, "disable")

	err := r.Apply(context.Background(),
		rotator.Credentials{Username: user, Password: initialPassword},
		rotator.Credentials{Username: "someone-else", Password: "whatever"},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "username change not supported")

	// Nothing should have touched the database at all for this case - the
	// check happens before any connection is opened.
	db := connect(t, host, port, database, user, initialPassword)
	assert.NoError(t, db.PingContext(context.Background()))
}
