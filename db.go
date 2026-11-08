package pgtest

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"github.com/stretchr/testify/require"
)

// dbOpsMu synchronizes administrative database operations (DROP/CREATE DATABASE)
// across parallel tests within the same test process.
var dbOpsMu sync.Mutex

// sharedPrimaryDB is the connection pool to the primary test database created during setup.
// Initialized by [StartPostgresContainer] before [CreateDBTemplate] is called.
var sharedPrimaryDB *sql.DB

// sharedPrimaryDSN stores the connection DSN for the primary database.
// Used as a base URL to derive per-test connection strings by swapping the database name.
var sharedPrimaryDSN string

// nonAlphaNumRegex matches any character that is not an ASCII letter, digit, or underscore.
// Used to sanitize arbitrary test names into valid PostgreSQL identifiers.
var nonAlphaNumRegex = regexp.MustCompile(`\W`)

func isRetryableDBErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "connection reset by peer") ||
		strings.Contains(s, "connection refused") ||
		strings.Contains(s, "no connection to the server") ||
		strings.Contains(s, "terminating connection") ||
		strings.Contains(s, "SSL connection has been closed unexpectedly") ||
		strings.Contains(s, "the database system is starting up") ||
		strings.Contains(s, "source database is being accessed by other users") ||
		strings.Contains(s, "database is being accessed by other users")
}

func retryDBOp(maxAttempts int, op func() error) error {
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(50+attempt*100)*time.Millisecond +
				time.Duration(rand.Intn(100))*time.Millisecond
			time.Sleep(backoff)
		}
		lastErr = op()
		if lastErr == nil {
			return nil
		}
		if !isRetryableDBErr(lastErr) {
			return lastErr
		}
	}
	return lastErr
}

// NewDB provisions an isolated database cloned from the template snapshot created by
// [CreateDBTemplate] and returns an active [*sql.DB] connection pool to it.
//
// The database is uniquely identified by the sanitized dbName (e.g. passing t.Name()).
// When the test finishes, [testing.T.Cleanup] automatically closes the connection pool,
// terminates any lingering connections, and drops the test database.
//
// Prerequisites:
//   - [StartPostgresContainer] must have been called in TestMain.
//   - [CreateDBTemplate] must have been called in TestMain after running migrations.
func NewDB(t *testing.T, dbName string) *sql.DB {
	t.Helper()

	require.NotNil(t, sharedAdminDB,
		"sharedAdminDB is nil: call pgtest.StartPostgresContainer() in TestMain before using NewDB")

	require.NotEmpty(t, dbName,
		"dbName must not be empty")

	dbName = nonAlphaNumRegex.ReplaceAllString(strings.ToLower(dbName), "_")

	adminPool := sharedAdminDB

	templateDBName, err := dbNameFromDSN(sharedPrimaryDSN)
	require.NoError(t, err, "failed to extract database name from primary DSN")

	templateName := fmt.Sprintf("%s_template", templateDBName)

	dbOpsMu.Lock()
	defer dbOpsMu.Unlock()

	_, _ = adminPool.ExecContext(context.Background(), fmt.Sprintf(`
		SELECT pg_terminate_backend(pid)
		FROM   pg_stat_activity
		WHERE  datname = '%s'
		AND    pid <> pg_backend_pid()
	`, dbName))

	err = retryDBOp(5, func() error {
		_, dropErr := adminPool.ExecContext(context.Background(), fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", dbName))
		return dropErr
	})
	require.NoError(t, err, "failed to drop leftover database %q before test setup", dbName)

	err = retryDBOp(5, func() error {
		_, createErr := adminPool.ExecContext(context.Background(), fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", dbName, templateName))
		return createErr
	})
	require.NoError(t, err, "failed to create test database %q from template %q", dbName, templateName)

	newConnString, err := replaceDatabaseInDSN(sharedPrimaryDSN, dbName)
	require.NoError(t, err, "failed to build connection string for test database %q", dbName)

	newDB, err := sql.Open("postgres", newConnString)
	require.NoError(t, err, "failed to open connection pool for test database %q", dbName)

	require.NoError(t, newDB.PingContext(context.Background()),
		"failed to ping test database %q", dbName)

	t.Cleanup(func() {
		_ = newDB.Close()
		dbOpsMu.Lock()
		defer dbOpsMu.Unlock()
		_, _ = adminPool.ExecContext(context.Background(), fmt.Sprintf(`
			SELECT pg_terminate_backend(pid)
			FROM   pg_stat_activity
			WHERE  datname = '%s'
			AND    pid <> pg_backend_pid()
		`, dbName))
		_ = retryDBOp(5, func() error {
			_, dropErr := adminPool.ExecContext(context.Background(), fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", dbName))
			return dropErr
		})
	})

	return newDB
}

// dbNameFromDSN extracts the database name from a PostgreSQL DSN URL.
func dbNameFromDSN(dsn string) (string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse DSN: %w", err)
	}
	name := strings.TrimPrefix(parsed.Path, "/")
	if name == "" {
		return "", fmt.Errorf("empty database name in DSN")
	}
	return name, nil
}

// replaceDatabaseInDSN returns a copy of dsn with the database path swapped for newName.
func replaceDatabaseInDSN(dsn, newName string) (string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse DSN: %w", err)
	}
	parsed.Path = "/" + newName
	return parsed.String(), nil
}
