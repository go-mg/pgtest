// Package pgtest provides lightweight, robust testing utilities for spinning up
// isolated PostgreSQL databases backed by Docker containers.
//
// Built exclusively on Go's standard "database/sql" package and the "github.com/lib/pq"
// driver, pgtest enables high-throughput, parallel integration testing without
// proprietary database abstractions or data collisions between test cases.
//
// # Key Features
//
//   - Zero custom ORM/wrapper dependencies: exposes standard [*sql.DB] connection pools.
//   - Per-test database isolation: each test receives an independent database cloned from a template.
//   - Safe for parallel testing: fully supports [testing.T.Parallel] across multiple tests.
//   - Fast container reuse: keeps the PostgreSQL container running between test runs for rapid TDD iterations.
//   - Automatic cleanup: registers [testing.T.Cleanup] handlers to drop test databases when tests finish.
//
// # Typical Workflow
//
// 1. [StartPostgresContainer] is called once inside TestMain to start (or reuse) a Docker container.
// 2. Database migrations and schema setups are applied to the primary database ([DockerizedPostgres.DB]).
// 3. [CreateDBTemplate] snapshots the migrated database into a PostgreSQL template.
// 4. Individual tests call [NewDB] to obtain a freshly cloned database with schema and seed data ready.
// 5. When the test completes, [NewDB]'s cleanup hook automatically terminates active sessions and drops the database.
// 6. When the test suite finishes, the teardown function returned by [StartPostgresContainer] cleans up primary resources.
//
// # Example in TestMain
//
//	package mypackage_test
//
//	import (
//		"context"
//		"database/sql"
//		"os"
//		"testing"
//
//		_ "github.com/lib/pq"
//		"github.com/stretchr/testify/require"
//		"github.com/go-mg/pgtest"
//	)
//
//	var testContainer *pgtest.DockerizedPostgres
//
//	func TestMain(m *testing.M) {
//		container, teardown, err := pgtest.StartPostgresContainer(pgtest.DockerContainerConfig{
//			Version:        "17.4-alpine",
//			ReuseContainer: true,
//			Expire:         3600, // 1 hour TTL safety net
//		})
//		if err != nil {
//			panic(err)
//		}
//
//		testContainer = container
//
//		// Apply migrations to the primary database before creating the template.
//		// For example:
//		// if err := runMigrations(container.DB); err != nil {
//		//     panic(err)
//		// }
//
//		// Snapshot the migrated database state as a reusable template.
//		if err = pgtest.CreateDBTemplate(); err != nil {
//			panic(err)
//		}
//
//		code := m.Run()
//		teardown()
//		os.Exit(code)
//	}
//
//	func TestUserCreation(t *testing.T) {
//		t.Parallel()
//
//		db := pgtest.NewDB(t, t.Name())
//
//		ctx := context.Background()
//		_, err := db.ExecContext(ctx, "CREATE TABLE users (id serial PRIMARY KEY, name text NOT NULL)")
//		require.NoError(t, err)
//
//		res, err := db.ExecContext(ctx, "INSERT INTO users (name) VALUES ($1)", "alice")
//		require.NoError(t, err)
//
//		rows, err := res.RowsAffected()
//		require.NoError(t, err)
//		require.Equal(t, int64(1), rows)
//	}
//
// # Container Reuse
//
// Setting [DockerContainerConfig.ReuseContainer] to true optimizes test cycles by retaining
// a fixed container named "pg-test-container". Successive test runs connect directly to the
// running container rather than restarting Docker images, saving significant CPU and wall-clock time.
//
// # Safety and Garbage Collection
//
// In the event of process termination, crashes, or aborted debugging sessions:
//   - [DockerContainerConfig.Expire] applies a hard TTL (in seconds) to the container via Docker.
//   - [NewDB] defensively terminates active connections and drops any preexisting database
//     matching the test's identifier prior to creating a new one.
//   - [testing.T.Cleanup] guarantees that test databases are dropped immediately upon test completion.
package pgtest
