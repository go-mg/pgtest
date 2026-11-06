// Package pgtest provides test helpers for spinning up isolated PostgreSQL
// databases backed by a Docker container.
//
// # Typical usage in TestMain
//
//	func TestMain(m *testing.M) {
//		container, teardown, err := pgtest.StartPostgresContainer(pgtest.DockerContainerConfig{
//			Version:        "15-alpine",
//			ReuseContainer: true,
//		})
//		if err != nil {
//			panic(err)
//		}
//
//		// Run migrations against the main database here, before creating the template.
//		// e.g.: runMigrations(container.DB)
//
//		if err = pgtest.CreateDBTemplate(); err != nil {
//			panic(err)
//		}
//
//		_ = container
//		code := m.Run()
//		teardown()
//		os.Exit(code)
//	}
//
// Each test then calls [NewDB] to get its own isolated database cloned from the template.
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

var dbOpsMu sync.Mutex

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

// concurrentConn is the shared "mother-connection" pool used to create and drop
// per-test databases. It is intentionally a package-level variable: tests run in
// separate processes (one per package), so there is no cross-package sharing.
// It is set by [StartPostgresContainer] and must be initialized before calling [NewDB].
var concurrentConn *sql.DB

// concurrentConnDSN stores the connection string used by concurrentConn so we
// can derive per-test DSNs by swapping the database name. sql.DB does not
// expose its original DSN.
var concurrentConnDSN string

// nonAlphaRegex matches any character that is not a letter, digit, or underscore.
// Used to sanitize arbitrary test names into valid PostgreSQL identifiers.
var nonAlphaRegex = regexp.MustCompile(`[\W]`)

// NewDB creates a fresh database named after the sanitized dbName and returns a
// connection pool to it. The database is cloned from the template created by
// [CreateDBTemplate], so it already contains the schema and any seed data applied
// before the template was snapshotted.
//
// The database is automatically dropped when the test finishes via [testing.T.Cleanup].
//
// Prerequisites:
//   - [StartPostgresContainer] must have been called (sets the internal connection).
//   - [CreateDBTemplate] must have been called after any desired migrations.
func NewDB(t *testing.T, dbName string) *sql.DB {
	t.Helper()

	require.NotNil(t, concurrentConnDB,
		"concurrentConnDB is nil: call pgtest.StartPostgresContainer() in TestMain before using NewDB")

	require.NotEmpty(t, dbName,
		"dbName must not be empty")

	dbName = nonAlphaRegex.ReplaceAllString(strings.ToLower(dbName), "_")

	adminPool := concurrentConnDB

	templateDBName, err := dbNameFromDSN(concurrentConnDSN)
	require.NoError(t, err, "failed to extract database name from concurrent connection DSN")

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

	newConnString, err := replaceDatabaseInDSN(concurrentConnDSN, dbName)
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
