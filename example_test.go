package pgtest_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/go-mg/pgtest"
	_ "github.com/lib/pq"
)

// Example demonstrates standard usage of pgtest across TestMain and individual tests.
func Example() {
	// In TestMain:
	//
	//	func TestMain(m *testing.M) {
	//		container, teardown, err := pgtest.StartPostgresContainer(pgtest.DockerContainerConfig{
	//			Version:        "17.4-alpine",
	//			ReuseContainer: true,
	//		})
	//		if err != nil {
	//			panic(err)
	//		}
	//
	//		// Run migrations on container.DB if necessary:
	//		// _ = runMigrations(container.DB)
	//
	//		if err = pgtest.CreateDBTemplate(); err != nil {
	//			panic(err)
	//		}
	//
	//		code := m.Run()
	//		teardown()
	//		os.Exit(code)
	//	}
	//
	// In test functions:
	//
	//	func TestCreateOrder(t *testing.T) {
	//		t.Parallel()
	//		db := pgtest.NewDB(t, t.Name())
	//		// Use db (*sql.DB) normally
	//	}

	fmt.Println("pgtest provides isolated postgres databases for testing")
	// Output:
	// pgtest provides isolated postgres databases for testing
}

// ExampleStartPostgresContainer demonstrates starting or reusing a PostgreSQL
// Docker container in TestMain.
func ExampleStartPostgresContainer() {
	// Start or reuse a container
	container, teardown, err := pgtest.StartPostgresContainer(pgtest.DockerContainerConfig{
		Version:        "17.4-alpine",
		ReuseContainer: true,
		Expire:         3600,
	})
	if err != nil {
		panic(err)
	}
	defer teardown()

	_ = container.DB
	_ = container.Port
}

// ExampleCreateDBTemplate demonstrates snapshotting the primary database into a template.
func ExampleCreateDBTemplate() {
	// Snapshot the current schema as a template for NewDB
	if err := pgtest.CreateDBTemplate(); err != nil {
		panic(err)
	}
}

// ExampleNewDB demonstrates how test functions request an isolated database.
func ExampleNewDB() {
	// Provided inside a real test: func TestMyFeature(t *testing.T)
	var t *testing.T

	// Get an isolated database instance cloned from the template
	db := pgtest.NewDB(t, "test_my_feature")

	_ = context.Background()
	_ = db
}
