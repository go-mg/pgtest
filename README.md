# pgtest

[![Go Reference](https://pkg.go.dev/badge/github.com/go-mg/pgtest.svg)](https://pkg.go.dev/github.com/go-mg/pgtest)
[![Go Report Card](https://goreportcard.com/badge/github.com/go-mg/pgtest)](https://goreportcard.com/report/github.com/go-mg/pgtest)
[![License](https://img.shields.io/github/license/go-mg/pgtest.svg)](LICENSE)

PostgreSQL integration testing utility for Go using Docker containers, built exclusively on standard library `*sql.DB` connection pools.

## Overview

`pgtest` simplifies running fast, isolated PostgreSQL integration tests in Go. It spins up a PostgreSQL container (or reuses an existing one), applies migrations to a primary database, creates a snapshot template, and provisions independent cloned databases for each test case.

- **No custom ORM/wrapper dependencies**: Uses standard library `*sql.DB`.
- **Driver**: `github.com/lib/pq` (imported automatically).
- **Per-test isolation**: Each test gets its own isolated physical database cloned from a template.
- **Parallel execution**: Safe for `t.Parallel()`.
- **Fast TDD cycles**: Supports container reuse across runs to eliminate startup overhead.
- **Automatic cleanup**: Leverages `testing.T.Cleanup` to drop per-test databases immediately.

## Installation

```bash
go get github.com/go-mg/pgtest
```

## Quick Start

```go
package mypackage_test

import (
    "context"
    "database/sql"
    "os"
    "testing"

    _ "github.com/lib/pq"
    "github.com/stretchr/testify/require"

    "github.com/go-mg/pgtest"
)

var testContainer *pgtest.DockerizedPostgres

func TestMain(m *testing.M) {
    // 1. Start or connect to a running PostgreSQL container
    container, teardown, err := pgtest.StartPostgresContainer(pgtest.DockerContainerConfig{
        Version:        "17.4-alpine",
        ReuseContainer: true,
        Expire:         3600, // 1 hour TTL safety net
    })
    if err != nil {
        panic(err)
    }

    testContainer = container

    // 2. (Optional) Run schema migrations on the primary database
    // err = runMigrations(container.DB)
    // if err != nil { panic(err) }

    // 3. Create the template database snapshot for all tests to clone
    if err = pgtest.CreateDBTemplate(); err != nil {
        panic(err)
    }

    // 4. Run tests and clean up
    code := m.Run()
    teardown()
    os.Exit(code)
}

func TestCreateUser(t *testing.T) {
    t.Parallel() // Safe for concurrent execution

    // Get an isolated database instance cloned from the template
    db := pgtest.NewDB(t, t.Name())

    ctx := context.Background()
    _, err := db.ExecContext(ctx, `
        CREATE TABLE users (
            id   serial PRIMARY KEY,
            name text NOT NULL
        )
    `)
    require.NoError(t, err)

    res, err := db.ExecContext(ctx, "INSERT INTO users (name) VALUES ($1)", "alice")
    require.NoError(t, err)

    affected, err := res.RowsAffected()
    require.NoError(t, err)
    require.Equal(t, int64(1), affected)
}
```

## Core API

### `StartPostgresContainer`

Starts (or connects to) a PostgreSQL Docker container and provisions the primary database.

```go
func StartPostgresContainer(cfg DockerContainerConfig) (*DockerizedPostgres, func(), error)
```

The returned `teardownFn` closes connections and, when `ReuseContainer: false`, removes the container. Call it at the end of `TestMain` or with `defer`.

#### Configuration: `DockerContainerConfig`

```go
type DockerContainerConfig struct {
    // ReuseContainer keeps the container alive between test runs and shares it
    // across packages running in parallel. Useful for faster local development.
    // When false, the container is removed during teardown.
    ReuseContainer bool

    // DBName is the name of the primary PostgreSQL database created inside the
    // container. If empty, a unique name is generated automatically.
    DBName string

    // Version is the Postgres Docker image tag (e.g. "17.4-alpine").
    // Defaults to "17.4-alpine" when empty.
    Version string

    // Image is the Docker repository/image name (without tag). Defaults to
    // "public.ecr.aws/docker/library/postgres" when empty.
    Image string

    // Expire sets a hard TTL (in seconds) on the container. This is a safety net
    // for situations where the test process terminates unexpectedly before teardown runs.
    // Zero means no expiry.
    Expire uint

    // Cmd is an optional list of arguments passed directly to the container's
    // entrypoint (the postgres binary). Useful for server-level parameters
    // like shared_preload_libraries.
    Cmd []string
}
```

### `DockerizedPostgres`

Returned by `StartPostgresContainer`, providing access to the container:

```go
type DockerizedPostgres struct {
    // DB is connected to the primary database created inside the container.
    // Use this connection pool to run migrations before calling CreateDBTemplate().
    DB *sql.DB

    // Port is the host-side TCP port mapped to container port 5432.
    Port string
}
```

### `CreateDBTemplate`

Snapshots the current state of the primary database into a PostgreSQL template database (`<dbName>_template`).

```go
func CreateDBTemplate() error
```

Must be called in `TestMain` **after** `StartPostgresContainer` and **after** running any migrations.

### `NewDB`

Provisions an isolated database named after `dbName` (sanitized) cloned from the template.

```go
func NewDB(t *testing.T, dbName string) *sql.DB
```

#### How `NewDB` Operates

1. **Identifier Sanitization**: Converts the name to lowercase and replaces non-alphanumeric characters with `_`.
2. **Defensive Cleanup**: Drops any preexisting database matching the name in case a previous process crashed.
3. **Template Cloning**: Creates the new database from the template (`CREATE DATABASE {name} TEMPLATE {dbName}_template`).
4. **Connection Pool**: Establishes a new `*sql.DB` connection pool to the test database and validates connectivity with `PingContext`.
5. **Automatic Cleanup**: Registers a `t.Cleanup()` callback to close the pool and drop the test database when the test ends.

## Container Reuse

### Why Reuse?

- **Performance**: Eliminates Docker container spin-up and tear-down latency on every run.
- **Resource Efficiency**: Keeps Docker memory and CPU footprint constant.
- **Developer Experience**: Drastically speeds up local TDD feedback loops.

### How It Works

When `ReuseContainer: true`:

1. Uses a fixed container name: `pg-test-container`.
2. Checks if a running container with this name already exists; reuses it if found.
3. If stopped or stale, removes and starts a fresh one.
4. Uses backoff retries to handle concurrent starts cleanly.

> **Note**: Container reuse is completely safe because every test operates on its own dedicated database inside PostgreSQL.

### Database Architecture Flow

```text
┌──────────────────────────────────────────────┐
│  Docker Container: pg-test-container         │
│                                              │
│  ┌───────────────────────────────────────┐   │
│  │  Default Database: "postgres"         │   │  ← used for administrative CREATE/DROP
│  └───────────────────────────────────────┘   │
│                                              │
│  ┌───────────────────────────────────────┐   │
│  │  Primary Database (e.g. db_xyz_1)     │   │  ← where migrations are applied
│  └───────────────────────────────────────┘   │
│                                              │
│  ┌───────────────────────────────────────┐   │
│  │  {dbName}_template                    │   │  ← post-migration template snapshot
│  └───────────────────────────────────────┘   │
│                                              │
│  ┌───────────────────────────────────────┐   │
│  │  test_feature_a                       │   │  ← Test 1 (cloned from template)
│  └───────────────────────────────────────┘   │
│                                              │
│  ┌───────────────────────────────────────┐   │
│  │  test_feature_b                       │   │  ← Test 2 (parallel clone)
│  └───────────────────────────────────────┘   │
│                                              │
│  ┌───────────────────────────────────────┐   │
│  │  test_feature_c                       │   │  ← Test 3 (parallel clone)
│  └───────────────────────────────────────┘   │
└──────────────────────────────────────────────┘
```

## Technical Architecture

### Shared State Management

The package maintains internal package-level variables:

```go
var (
    // Admin connection pool connected to default "postgres" database.
    sharedAdminDB *sql.DB

    // Connection pool connected to primary test database.
    sharedPrimaryDB *sql.DB

    // Base DSN used to derive per-test connection strings.
    sharedPrimaryDSN string

    // Container ID used to execute pg_dump/psql inside the container.
    sharedContainerID string
)
```

`StartPostgresContainer` initializes these variables for `NewDB` and `CreateDBTemplate`. Since Go test packages run in isolated processes, package-level state is isolated per package.

### Default Credentials

Hardcoded credentials configured for the container environment:

- **User**: `postgres`
- **Password**: `postgres`
- **SSL**: disabled (`sslmode=disable`)

### Two-Stage Cleanup

1. **Per Test** (via `t.Cleanup` in `NewDB`):
   - Closes the test's `*sql.DB` connection pool (`db.Close()`).
   - Terminates active backend sessions on the test database.
   - Executes `DROP DATABASE IF EXISTS {dbName} WITH (FORCE)` via the admin connection pool.

2. **Global Suite** (via `teardownFn` returned by `StartPostgresContainer`):
   - Closes the primary database connection pool.
   - Drops the primary database and the template database.
   - Closes the admin connection pool.
   - If `!cfg.ReuseContainer`, removes the Docker container.

## Requirements

- **Docker daemon**: Installed and running locally.
- **Go**: 1.22+ (configured with Go modules).
- **PostgreSQL driver**: `github.com/lib/pq` (imported automatically).

## Troubleshooting

### Container Fails to Start
- Verify Docker daemon is running: `docker ps`.
- Check port availability and container logs: `docker logs pg-test-container`.

### Orphan Container After Aborted Run
If `Expire` was not configured and a debug run was terminated forcefully:
```bash
docker rm -f pg-test-container
```

### `sharedAdminDB is nil` Error
Ensure `StartPostgresContainer` is called in `TestMain` before `NewDB` or `CreateDBTemplate`.

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.
