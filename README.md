# pgtest

PostgreSQL testing utility package using Docker containers, built exclusively on Go's standard `database/sql` package.

## Overview

This package provides tools to spin up and manage PostgreSQL Docker containers for testing, enabling parallel test execution with complete isolation through separate databases.

- No dependencies on `pgx` or proprietary wrappers: uses standard `*sql.DB`
- Driver: `github.com/lib/pq` (registered via blank import)
- Per-test isolation: each test receives an isolated database cloned from a template

## Core Components

### StartPostgresContainer

Starts (or reuses) a PostgreSQL Docker container and creates the primary database. It is the entry point that configures the infrastructure shared by all tests in the package.

```go
func StartPostgresContainer(cfg DockerContainerConfig) (*DockerizedPostgres, func(), error)
```

The returned `teardownFn` must be called at the end (via `defer` or in `TestMain` after `m.Run()`) to close connections and, when `ReuseContainer: false`, remove the container.

#### Configuration

```go
type DockerContainerConfig struct {
    // ReuseContainer keeps the container alive between test runs and shares it
    // across packages running in parallel. Useful for faster local development.
    // When false, the container is removed after teardown.
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
    // for situations where the process is killed before teardown runs (e.g. during
    // debugging). Zero means no expiry.
    Expire uint

    // Cmd is an optional list of arguments passed directly to the container's
    // entrypoint (the postgres binary). Useful for server-level parameters
    // like shared_preload_libraries.
    Cmd []string
}
```

#### TestMain Usage Example

```go
func TestMain(m *testing.M) {
    container, teardown, err := pgtest.StartPostgresContainer(pgtest.DockerContainerConfig{
        Version:        "17.4-alpine",
        ReuseContainer: true,
    })
    if err != nil {
        panic(err)
    }

    // Run migrations on the primary database here, BEFORE creating the template.
    // e.g.: runMigrations(container.DB)

    if err = pgtest.CreateDBTemplate(); err != nil {
        panic(err)
    }

    _ = container
    code := m.Run()
    teardown()
    os.Exit(code)
}
```

### DockerizedPostgres

Struct returned by `StartPostgresContainer` containing the exposed resources:

```go
type DockerizedPostgres struct {
    // DB is connected to the primary database created inside the container.
    // Use this to run migrations before calling CreateDBTemplate().
    DB *sql.DB

    // Port is the host-side TCP port mapped to 5432 inside the container.
    Port string
}
```

### CreateDBTemplate

Takes a snapshot of the primary database into a template database (`{dbName}_template`). `NewDB` clones from this template, ensuring that every test starts with identical schema and seed data.

```go
func CreateDBTemplate() error
```

Must be called in `TestMain`, **after** `StartPostgresContainer` and **after** running any migrations you want every test database to inherit.

### NewDB

Creates an isolated database named after `dbName` (sanitized) and returns a `*sql.DB` connected to it. The database is cloned from the template created by `CreateDBTemplate`.

```go
func NewDB(t *testing.T, dbName string) *sql.DB
```

#### How It Works

1. **Name Sanitization**: The name is converted to lowercase and non-alphanumeric/underscore characters are replaced with `_` (via regex `[\W]`).
2. **Defensive Cleanup**: If a database with the same name exists (e.g. from an earlier run that crashed before cleanup), it is dropped before creation.
3. **Template Creation**: Clones from the template (`CREATE DATABASE {name} TEMPLATE {dbName}_template`) for fast snapshot provisioning.
4. **New Connection**: Opens a new `*sql.DB` using the same host/port/credentials by replacing the database name in the DSN, and verifies connectivity with `PingContext`.
5. **Automatic Cleanup**: Registers `t.Cleanup()` to close the test's `*sql.DB` connection pool and drop the test database.

#### Prerequisites

Before calling `NewDB` in any test:

- `StartPostgresContainer()` must have been called (sets up internal connection pools).
- `CreateDBTemplate()` must have been called after running migrations.

#### Example Usage

```go
func TestMyFeature(t *testing.T) {
    t.Parallel() // Can run in parallel with other tests

    db := pgtest.NewDB(t, t.Name())

    ctx := context.Background()
    _, err := db.ExecContext(ctx, "CREATE TABLE users (id serial PRIMARY KEY, name VARCHAR(255))")
    require.NoError(t, err)

    res, err := db.ExecContext(ctx, "INSERT INTO users (name) VALUES ($1)", "alice")
    require.NoError(t, err)

    n, _ := res.RowsAffected()
    require.Equal(t, int64(1), n)
}
```

## Container Reuse

### Why Reuse?

- **Performance**: Eliminates overhead of spinning up and tearing down Docker containers on every test run.
- **Resource Efficiency**: Reduces Docker CPU and memory footprint.
- **Speed**: Significantly accelerates test execution in TDD cycles.

### How It Works

When `ReuseContainer: true`:

1. The container uses a fixed name: `pg-test-container`.
2. On initial startup, it checks if a running container with this name already exists.
3. If found and running, it reuses it; otherwise, it creates a new one.
4. If the container exists but is stopped, it removes the stale container and starts a fresh one.
5. Employs backoff retries to handle race conditions when multiple test packages start concurrently.

> Container reuse is completely safe because each test receives **its own isolated database** within the PostgreSQL instance. The shared container acts only as the shared server process.

### Database Creation Flow

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
│  │  test_myfeature_1                     │   │  ← Test 1 (cloned from template)
│  └───────────────────────────────────────┘   │
│                                              │
│  ┌───────────────────────────────────────┐   │
│  │  test_myfeature_2                     │   │  ← Test 2 (parallel clone)
│  └───────────────────────────────────────┘   │
│                                              │
│  ┌───────────────────────────────────────┐   │
│  │  test_myfeature_3                     │   │  ← Test 3 (parallel clone)
│  └───────────────────────────────────────┘   │
└──────────────────────────────────────────────┘
```

## Full Usage Pattern

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

var container *pgtest.DockerizedPostgres

func TestMain(m *testing.M) {
    // Start container ONCE for all tests in this package
    c, teardown, err := pgtest.StartPostgresContainer(pgtest.DockerContainerConfig{
        Version:        "17.4-alpine",
        ReuseContainer: true,
        Expire:         3600, // 1h TTL as a safety net
    })
    if err != nil {
        panic(err)
    }

    container = c

    // (Optional) Run migrations on primary database
    // err = runMigrations(container.DB)
    // if err != nil { panic(err) }

    // Snapshot the current state as a template for all tests
    if err = pgtest.CreateDBTemplate(); err != nil {
        panic(err)
    }

    code := m.Run()
    teardown() // closes connections and (if !ReuseContainer) removes container

    os.Exit(code)
}

func TestCreateUser(t *testing.T) {
    t.Parallel()

    ctx := context.Background()
    db := pgtest.NewDB(t, t.Name())

    _, err := db.ExecContext(ctx, `
        CREATE TABLE users (
            id   serial PRIMARY KEY,
            name VARCHAR(255) NOT NULL
        )
    `)
    require.NoError(t, err)

    res, err := db.ExecContext(ctx,
        "INSERT INTO users (name) VALUES ($1), ($2)",
        "alice", "bob",
    )
    require.NoError(t, err)

    affected, _ := res.RowsAffected()
    require.Equal(t, int64(2), affected)

    var count int
    err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&count)
    require.NoError(t, err)
    require.Equal(t, 2, count)
}

func TestOrders(t *testing.T) {
    t.Parallel() // Runs concurrently with TestCreateUser without interference

    ctx := context.Background()
    db := pgtest.NewDB(t, t.Name())

    _, err := db.ExecContext(ctx, `
        CREATE TABLE orders (
            id    serial PRIMARY KEY,
            total DECIMAL(10,2)
        )
    `)
    require.NoError(t, err)

    // ... order test queries ...
}
```

## Advantages

### Complete Isolation

- Each test runs against its own physical database
- Zero data interference between tests
- Safe for `t.Parallel()` execution

### Performance

- Container reuse across test runs (fast TDD feedback loops)
- Fast database templating for instant cloning
- Parallel tests run concurrently against the same Postgres server instance

### Simplicity

- Standard `*sql.DB` API — no custom wrappers or abstractions to learn
- `ExecContext`, `QueryContext`, `QueryRowContext`, `BeginTx`, etc. work as expected
- Automatic cleanup via `t.Cleanup`

### Debugging

- Isolated databases make inspecting failed test state straightforward
- `Expire` option prevents orphan containers on unexpected termination
- Fixed container naming for easy manual inspection via Docker CLI

## Technical Details

### Shared Connection Pools

The package manages internal package-level variables:

```go
var (
    // Admin connection pool to the default "postgres" database for CREATE/DROP operations.
    concurrentConnDB *sql.DB

    // Connection pool to the primary test database.
    concurrentConn *sql.DB

    // Original DSN used to derive per-test database connection strings.
    concurrentConnDSN string
)
```

`StartPostgresContainer` initializes these variables for use by `NewDB` and `CreateDBTemplate`. Since Go test packages run in separate processes, there is no cross-package variable sharing.

### DSN Manipulation

To generate per-test database connection strings, the package replaces only the database name in the connection URL path while preserving host, port, credentials, and query parameters:

- `dbNameFromDSN(dsn string) (string, error)` — extracts the database name from the URL path
- `replaceDatabaseInDSN(dsn, newName string) (string, error)` — returns a new DSN with the updated database name
- `getPostgresConnString(port, dbName string) string` — constructs standard PostgreSQL DSN: `postgres://postgres:postgres@localhost:{port}/{dbName}?sslmode=disable`

### Default Credentials

Hardcoded credentials for the container environment (`POSTGRES_USER` / `POSTGRES_PASSWORD`):

- User: `postgres`
- Password: `postgres`
- SSL: disabled (`sslmode=disable`)

This configuration is intended for local ephemeral test environments.

### Two-Stage Cleanup

**Per Test** (via `t.Cleanup` in `NewDB`):

1. Closes the test's `*sql.DB` connection pool (`db.Close()`)
2. Terminates active backends and executes `DROP DATABASE IF EXISTS {dbName} WITH (FORCE)` via the admin connection pool

**Global** (via `teardownFn` returned by `StartPostgresContainer`):

1. Closes the primary database connection (`primaryDB.Close()`)
2. Drops the primary database (`DROP DATABASE IF EXISTS {cfg.DBName}`)
3. Drops the template database (`DROP DATABASE IF EXISTS {cfg.DBName}_template`)
4. Closes the admin connection to the default `postgres` database
5. If `!cfg.ReuseContainer`, removes the Docker container (`dockerResource.Close()`)

## Requirements

- Docker daemon installed and running
- Network access to pull the PostgreSQL Docker image (if not cached locally)
- PostgreSQL driver registered: `github.com/lib/pq` (imported by `pgtest` directly)

## Troubleshooting

### Container Fails to Start

- Check `docker ps` and `docker logs pg-test-container`
- Verify host port availability and firewall/port mapping settings
- Check Docker daemon disk space and permissions

### Slow Tests

- Set `ReuseContainer: true` in your configuration
- Adjust parallelism flag if resource constrained (`go test -parallel=N`)
- Consider SSD storage for Docker data

### Database In-Use Errors During CREATE/DROP

Occurs if active sessions remain connected to the database. Ensure that:

- Goroutines do not leak open connections after the test finishes
- `*sql.DB` instances returned by `NewDB` are properly closed at test cleanup

### Orphan Container After Crash/Debug Kill

If `Expire` was not set or you need to manually clean up:

```bash
docker rm -f pg-test-container
```

### `concurrentConnDB is nil` Error

Occurs if `NewDB` or `CreateDBTemplate` is called before `StartPostgresContainer`. Ensure the execution sequence in `TestMain`:

1. `StartPostgresContainer`
2. (run migrations)
3. `CreateDBTemplate`
4. `m.Run()`
