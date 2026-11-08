# Development Log (DevLog) & Changelog

Record of architectural decisions, technical evolution, and release notes for **pgtest**.

---

## [v0.1.0] - 2026-11-08

### Context and Motivation
Database integration tests often face a trade-off between **speed** and **isolation**:
1. Running migrations and seeds for each individual test slows down the test suite and consumes excessive CPU and I/O.
2. Sharing a single database with truncated transactions (`TRUNCATE`/`ROLLBACK`) prevents true parallel test execution (`t.Parallel()`) and breaks with manual commits or concurrency.
3. Directly cloning an active PostgreSQL database (`CREATE DATABASE ... TEMPLATE`) fails when open connections exist.

### Architecture and Engineering Decisions
- **Smart Container Reuse (`StartPostgresContainer`)**:
  - Mechanism that checks whether the `pg-test-container` container already exists and is healthy on the target port before creating a new one.
  - Drastic reduction of startup overhead during continuous local development.
- **Single-Pass Schema Snapshot via Streaming (`CreateDBTemplate`)**:
  - Generation of an isolated base template (`template0` + `pg_dump | psql`) once at the start of the test suite (`TestMain`).
  - Captures the initial post-migration state without having to drop active pool connections.
- **Instantaneous Per-Test Provisioning (`NewDB`)**:
  - Disk block-level cloning via `CREATE DATABASE <test_db> TEMPLATE <template_db>`.
  - Millisecond-range execution (~2–10ms) per dedicated database.
  - Deterministic cleanup registration via `t.Cleanup()` with forced termination of active connections.
- **Custom Images and Server Startup Parameters**:
  - Support for local Docker builds to enable native extensions (e.g., `pg_cron`, `PostGIS`, `pgvector`) and startup arguments via `Cmd`.

### Standardization and Code Quality
- Comprehensive canonical documentation in `doc.go` and runnable examples in `example_test.go` for rendering on `pkg.go.dev`.
- Refactored identifier and variable names for clarity and maintainability (`sharedPrimaryDB`, `sharedAdminDB`, `dbInstanceSeq`, `nonAlphaNumRegex`).
- Continuous Integration (CI) pipeline configured with GitHub Actions running formatting verification (`gofmt`), static analysis (`go vet`, `golangci-lint`), and race-detector tests (`go test -race`).
- Compatibility validated for the modern Go ecosystem.
