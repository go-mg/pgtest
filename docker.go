package pgtest

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os/exec"
	"sync/atomic"
	"time"

	_ "github.com/lib/pq" // postgres driver for database/sql
	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
)

const (
	// reusableContainerName is a fixed container name allowing multiple test packages
	// to share the same running container instead of spinning up duplicate instances.
	reusableContainerName        = "pg-test-container"
	postgresDefaultDB            = "postgres"
	defaultPostgresDockerVersion = "17.4-alpine"
	defaultDockerImage           = "public.ecr.aws/docker/library/postgres"
)

// DockerContainerConfig holds configuration options for [StartPostgresContainer].
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
	//
	// For locally-built images use the repository name passed to `docker build -t`:
	//   e.g. Image: "my-custom-postgres", Version: "17"
	Image string

	// Expire sets a hard TTL (in seconds) on the container. This is a safety net
	// for situations where the test process terminates unexpectedly before teardown runs.
	// Zero means no expiry.
	Expire uint

	// Cmd is an optional list of arguments passed directly to the container's
	// entrypoint (the `postgres` binary). This is the canonical way to configure
	// server-level parameters that must be set at startup, such as
	// `shared_preload_libraries`. Each item should be supplied as a separate
	// flag and value:
	//
	//   Cmd: []string{"-c", "shared_preload_libraries=pg_cron", "-c", "cron.database_name=test_main_postgres"}
	Cmd []string
}

// dbInstanceSeq generates monotonically increasing sequence numbers for unique database names.
var dbInstanceSeq int32

// sharedAdminDB is the administrative connection pool connected to the default "postgres" database.
// Used to execute CREATE DATABASE and DROP DATABASE statements outside active target sessions.
var sharedAdminDB *sql.DB

// sharedContainerID is the Docker container ID of the running PostgreSQL container.
// Required by [CreateDBTemplate] to execute pg_dump and psql inside the container.
var sharedContainerID string

// DockerizedPostgres holds the exposed database connection pool and host port for the running container.
type DockerizedPostgres struct {
	// DB is connected to the primary database created inside the container.
	// Use this connection pool to run migrations before calling [CreateDBTemplate].
	DB *sql.DB

	// Port is the host-side TCP port mapped to container port 5432.
	Port string
}

// StartPostgresContainer starts (or reuses) a PostgreSQL Docker container and
// creates the primary database specified in cfg. It initializes internal shared connections
// used by [NewDB] and [CreateDBTemplate].
//
// The returned teardown function must be called when testing finishes (typically via defer
// or after m.Run() in TestMain) to close connections and, if ReuseContainer is false, remove the container.
//
// Intended to be called once in TestMain, before running migrations and before calling [CreateDBTemplate].
//
// Example:
//
//	func TestMain(m *testing.M) {
//		container, teardown, err := pgtest.StartPostgresContainer(pgtest.DockerContainerConfig{
//			Version:        "17.4-alpine",
//			ReuseContainer: true,
//		})
//		if err != nil {
//			panic(err)
//		}
//		defer teardown()
//
//		// run migrations on container.DB...
//		if err = pgtest.CreateDBTemplate(); err != nil {
//			panic(err)
//		}
//		os.Exit(m.Run())
//	}
func StartPostgresContainer(cfg DockerContainerConfig) (dockerPostgres *DockerizedPostgres, teardownFn func(), err error) {
	dockerPool, err := dockertest.NewPool("")
	if err != nil {
		return nil, nil, fmt.Errorf("could not connect to docker: %w", err)
	}

	if err = dockerPool.Client.Ping(); err != nil {
		return nil, nil, fmt.Errorf("could not ping docker daemon: %w", err)
	}

	if cfg.Version == "" {
		cfg.Version = defaultPostgresDockerVersion
	}

	if cfg.DBName == "" {
		atomic.AddInt32(&dbInstanceSeq, 1)
		cfg.DBName = fmt.Sprintf("db_%d_%d", time.Now().UnixNano(), atomic.LoadInt32(&dbInstanceSeq))
	}

	if cfg.Image == "" {
		cfg.Image = defaultDockerImage
	}

	dockerResource, isFreshResource, err := getDockerPostgresResource(dockerPool, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to initialize postgres docker resource: %w", err)
	}

	dbPort := dockerResource.GetPort("5432/tcp")

	// The container may be up but Postgres might still be initializing.
	// Retry until a real connection can be established.
	if err = dockerPool.Retry(pingPostgresFn(dbPort)); err != nil {
		return nil, nil, fmt.Errorf("postgres did not become ready in time: %w", err)
	}

	if cfg.Expire != 0 && isFreshResource {
		if err = dockerResource.Expire(cfg.Expire); err != nil {
			return nil, nil, fmt.Errorf("failed to set container expiry: %w", err)
		}
	}

	// Connect to the default "postgres" database to be able to CREATE/DROP other databases.
	defaultConnString := getPostgresConnString(dbPort, "postgres")
	defaultDB, err := sql.Open("postgres", defaultConnString)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open default postgres database: %w", err)
	}
	defaultDB.SetMaxOpenConns(1)
	defaultDB.SetMaxIdleConns(1)
	if err = defaultDB.PingContext(context.Background()); err != nil {
		_ = defaultDB.Close()
		return nil, nil, fmt.Errorf("failed to ping default postgres database: %w", err)
	}

	// Create the primary test database.
	if err = createDB(cfg.DBName, defaultDB); err != nil {
		_ = defaultDB.Close()
		return nil, nil, err
	}

	primaryConnString := getPostgresConnString(dbPort, cfg.DBName)
	primaryDB, err := sql.Open("postgres", primaryConnString)
	if err != nil {
		_ = defaultDB.Close()
		return nil, nil, fmt.Errorf("failed to open test database %q: %w", cfg.DBName, err)
	}
	if err = primaryDB.PingContext(context.Background()); err != nil {
		_ = primaryDB.Close()
		_ = defaultDB.Close()
		return nil, nil, fmt.Errorf("failed to ping test database %q: %w", cfg.DBName, err)
	}

	// Expose the pool globally so NewDB and CreateDBTemplate can use it.
	sharedPrimaryDB = primaryDB
	sharedPrimaryDSN = primaryConnString
	sharedAdminDB = defaultDB
	if dockerResource.Container != nil {
		sharedContainerID = dockerResource.Container.ID
	}

	teardownFn = func() {
		_ = primaryDB.Close()
		_ = dropDB(cfg.DBName, defaultDB)
		_ = dropDB(fmt.Sprintf("%s_template", cfg.DBName), defaultDB)
		_ = defaultDB.Close()
		if !cfg.ReuseContainer {
			_ = dockerResource.Close()
		}
	}

	return &DockerizedPostgres{
		DB:   primaryDB,
		Port: dbPort,
	}, teardownFn, nil
}

// CreateDBTemplate snapshots the current state of the primary database into a
// PostgreSQL template database (<dbName>_template). [NewDB] clones from this
// template, so every test starts with an identical schema and seed data.
//
// Call this in TestMain after [StartPostgresContainer] and after running any
// migrations you want every test database to inherit.
//
// Implementation note: the snapshot is taken with pg_dump piped into psql
// (both executed inside the container) instead of CREATE DATABASE ... TEMPLATE.
// PostgreSQL refuses to clone a database that still has open sessions, and
// background workers (such as pg_cron scheduler) maintain persistent connections.
// Extensions like pg_cron are excluded from the dump because they cannot be cloned
// into auxiliary template databases.
func CreateDBTemplate() error {
	if sharedPrimaryDB == nil || sharedAdminDB == nil {
		return fmt.Errorf("shared database connections are nil: call StartPostgresContainer() before CreateDBTemplate()")
	}
	if sharedContainerID == "" {
		return fmt.Errorf("container ID is empty: call StartPostgresContainer() before CreateDBTemplate()")
	}

	dbName, err := dbNameFromDSN(sharedPrimaryDSN)
	if err != nil {
		return fmt.Errorf("failed to extract database name from DSN: %w", err)
	}
	templateName := fmt.Sprintf("%s_template", dbName)

	if err = retryDBOp(5, func() error {
		_, dropErr := sharedAdminDB.ExecContext(context.Background(),
			fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", templateName),
		)
		return dropErr
	}); err != nil {
		return fmt.Errorf("failed to drop pre-existing template database %q: %w", templateName, err)
	}

	// Create the template from template0 so it starts empty and without any
	// inherited sessions, then load the snapshot into it.
	if err = retryDBOp(5, func() error {
		_, createErr := sharedAdminDB.ExecContext(context.Background(),
			fmt.Sprintf("CREATE DATABASE %s TEMPLATE template0", templateName),
		)
		return createErr
	}); err != nil {
		return fmt.Errorf("failed to create empty template database %q: %w", templateName, err)
	}

	if err = snapshotDatabase(sharedContainerID, dbName, templateName); err != nil {
		return fmt.Errorf("failed to snapshot database %q into %q: %w", dbName, templateName, err)
	}

	return nil
}

// snapshotDatabase logically copies srcDB into dstDB by piping pg_dump into
// psql, both run inside the given Docker container. Unlike CREATE DATABASE ...
// TEMPLATE it succeeds even while srcDB has open sessions.
func snapshotDatabase(containerID, srcDB, dstDB string) error {
	dump := exec.Command("docker", "exec", containerID,
		"pg_dump", "-U", "postgres", "--no-owner", "--no-privileges",
		"--exclude-extension=pg_cron",
		"-d", srcDB,
	)
	load := exec.Command("docker", "exec", "-i", containerID,
		"psql", "-U", "postgres", "-q", "-v", "ON_ERROR_STOP=1", "-d", dstDB,
	)

	pipe, err := dump.StdoutPipe()
	if err != nil {
		return fmt.Errorf("pg_dump stdout pipe: %w", err)
	}
	load.Stdin = pipe

	var dumpErr, loadErr bytes.Buffer
	dump.Stderr = &dumpErr
	load.Stderr = &loadErr

	// Start the loader first so it is ready to consume the dump, then run
	// pg_dump and wait for both processes to finish.
	if err = load.Start(); err != nil {
		return fmt.Errorf("start psql against %q: %w", dstDB, err)
	}
	if err = dump.Run(); err != nil {
		_ = load.Wait()
		return fmt.Errorf("pg_dump %q: %w: %s", srcDB, err, bytes.TrimSpace(dumpErr.Bytes()))
	}
	if err = load.Wait(); err != nil {
		return fmt.Errorf("psql load into %q: %w: %s", dstDB, err, bytes.TrimSpace(loadErr.Bytes()))
	}

	return nil
}

// getDockerPostgresResource returns an existing running container (when
// ReuseContainer is true) or starts a new one. The boolean second return
// value is true when the returned resource was freshly created via
// [dockertest.Pool.RunWithOptions] (and therefore has its internal pool set,
// so methods like Expire are safe to call). It is false when the returned
// resource wraps an already-running container and the internal pool is nil.
func getDockerPostgresResource(dockerPool *dockertest.Pool, cfg DockerContainerConfig) (*dockertest.Resource, bool, error) {
	var containerName string
	if cfg.ReuseContainer {
		containerName = reusableContainerName
		container, inspectErr := dockerPool.Client.InspectContainer(containerName)
		if inspectErr == nil && container != nil && container.State.Running {
			return &dockertest.Resource{Container: container}, false, nil
		}
		if container != nil && !container.State.Running {
			_ = dockerPool.RemoveContainerByName(containerName)
		}
	}

	resource, runErr := dockerPool.RunWithOptions(&dockertest.RunOptions{
		Name:       containerName,
		Repository: cfg.Image,
		Tag:        cfg.Version,
		Env:        []string{"POSTGRES_USER=postgres", "POSTGRES_PASSWORD=postgres", "POSTGRES_DB=" + cfg.DBName},
		Cmd:        cfg.Cmd,
	}, func(c *docker.HostConfig) {
		c.AutoRemove = true
		c.RestartPolicy = docker.RestartPolicy{Name: "no"}
	})
	if runErr == nil {
		return resource, true, nil
	}

	if cfg.ReuseContainer {
		var container *docker.Container
		if retryErr := dockerPool.Retry(func() error {
			var inspectErr error
			container, inspectErr = dockerPool.Client.InspectContainer(reusableContainerName)
			if inspectErr == nil && container != nil {
				return nil
			}
			if inspectErr != nil {
				return inspectErr
			}
			return fmt.Errorf("container %q not found", reusableContainerName)
		}); retryErr == nil && container != nil {
			return &dockertest.Resource{Container: container}, false, nil
		}
	}

	return nil, false, fmt.Errorf("failed to start postgres container (image=%s:%s): %w",
		cfg.Image, cfg.Version, runErr)
}

// pingPostgresFn returns a retry-able function that attempts a single connection
// and ping against the Postgres instance on the given host port.
func pingPostgresFn(port string) func() error {
	return func() error {
		conn, err := sql.Open("postgres", getPostgresConnString(port, "postgres"))
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }()
		return conn.PingContext(context.Background())
	}
}

// terminateDBConnections forcefully terminates every open session on the
// given database.  This is required before DROP DATABASE because PostgreSQL
// refuses to drop a database that has any active connections.  It is safe to
// call for databases that do not exist (the UPDATE touches zero rows).
func terminateDBConnections(dbName string, db *sql.DB) error {
	_, err := db.ExecContext(context.Background(), fmt.Sprintf(`
		SELECT pg_terminate_backend(pid)
		FROM   pg_stat_activity
		WHERE  datname = '%s'
		AND    pid <> pg_backend_pid()
	`, dbName))
	if err != nil {
		return fmt.Errorf("failed to terminate connections on database %q: %w", dbName, err)
	}
	return nil
}

// dropDB drops the named database if it exists. Skips the default "postgres"
// database which cannot be dropped. It first terminates every open session on
// the target database because PostgreSQL refuses to DROP a database that is
// still being accessed.
func dropDB(dbName string, db *sql.DB) error {
	if dbName == postgresDefaultDB {
		return nil
	}
	_ = terminateDBConnections(dbName, db)
	err := retryDBOp(5, func() error {
		_, dropErr := db.ExecContext(context.Background(), fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", dbName))
		return dropErr
	})
	if err != nil {
		return fmt.Errorf("failed to drop database %q: %w", dbName, err)
	}
	return nil
}

// createDB drops (if exists) and recreates the named database. Skips the
// default "postgres" database. When ReuseContainer=true or when the DB was
// created by the container's POSTGRES_DB env, the database may already exist
// and a plain CREATE DATABASE would fail — we therefore tolerate the
// "duplicate_database" error (SQLSTATE 42P04) and treat the DB as ready.
func createDB(dbName string, db *sql.DB) error {
	if dbName == postgresDefaultDB {
		return nil
	}
	_ = dropDB(dbName, db)
	err := retryDBOp(5, func() error {
		_, createErr := db.ExecContext(context.Background(), fmt.Sprintf("CREATE DATABASE %s", dbName))
		return createErr
	})
	if err != nil {
		var pqErr interface {
			Error() string
			Sqlstate() string
		}
		if errors.As(err, &pqErr) && pqErr.Sqlstate() == "42P04" {
			return nil
		}
		return fmt.Errorf("failed to create database %q: %w", dbName, err)
	}
	return nil
}

// getPostgresConnString builds a DSN for the given host port and database name.
func getPostgresConnString(port, dbName string) string {
	return fmt.Sprintf("postgres://postgres:postgres@localhost:%s/%s?sslmode=disable", port, dbName)
}
