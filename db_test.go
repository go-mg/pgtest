package pgtest

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDBNameFromDSN(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		dsn         string
		expectedDB  string
		expectError bool
	}{
		{
			name:        "standard postgres url",
			dsn:         "postgres://user:pass@localhost:5432/mydb?sslmode=disable",
			expectedDB:  "mydb",
			expectError: false,
		},
		{
			name:        "postgres url without query parameters",
			dsn:         "postgres://user:pass@localhost:5432/my_test_db",
			expectedDB:  "my_test_db",
			expectError: false,
		},
		{
			name:        "empty path",
			dsn:         "postgres://user:pass@localhost:5432",
			expectError: true,
		},
		{
			name:        "only slash path",
			dsn:         "postgres://user:pass@localhost:5432/",
			expectError: true,
		},
		{
			name:        "invalid url syntax",
			dsn:         "://invalid-url",
			expectError: true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dbName, err := dbNameFromDSN(tc.dsn)
			if tc.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.expectedDB, dbName)
			}
		})
	}
}

func TestReplaceDatabaseInDSN(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		dsn         string
		newDB       string
		expectedDSN string
		expectError bool
	}{
		{
			name:        "swap database name preserving credentials and query",
			dsn:         "postgres://postgres:postgres@localhost:54321/primary_db?sslmode=disable",
			newDB:       "test_case_1",
			expectedDSN: "postgres://postgres:postgres@localhost:54321/test_case_1?sslmode=disable",
			expectError: false,
		},
		{
			name:        "swap database name on simple url",
			dsn:         "postgres://localhost:5432/old_db",
			newDB:       "new_db",
			expectedDSN: "postgres://localhost:5432/new_db",
			expectError: false,
		},
		{
			name:        "invalid url syntax",
			dsn:         "://invalid-url",
			newDB:       "test_db",
			expectError: true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res, err := replaceDatabaseInDSN(tc.dsn, tc.newDB)
			if tc.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.expectedDSN, res)
			}
		})
	}
}

func TestIsRetryableDBErr(t *testing.T) {
	t.Parallel()

	assert.False(t, isRetryableDBErr(nil))
	assert.False(t, isRetryableDBErr(errors.New("table not found")))
	assert.False(t, isRetryableDBErr(errors.New("syntax error at or near")))

	assert.True(t, isRetryableDBErr(errors.New("connection reset by peer")))
	assert.True(t, isRetryableDBErr(errors.New("connection refused")))
	assert.True(t, isRetryableDBErr(errors.New("no connection to the server")))
	assert.True(t, isRetryableDBErr(errors.New("terminating connection")))
	assert.True(t, isRetryableDBErr(errors.New("SSL connection has been closed unexpectedly")))
	assert.True(t, isRetryableDBErr(errors.New("the database system is starting up")))
	assert.True(t, isRetryableDBErr(errors.New("source database is being accessed by other users")))
	assert.True(t, isRetryableDBErr(errors.New("database is being accessed by other users")))
}

func TestGetPostgresConnString(t *testing.T) {
	t.Parallel()

	dsn := getPostgresConnString("54321", "my_db")
	assert.Equal(t, "postgres://postgres:postgres@localhost:54321/my_db?sslmode=disable", dsn)
}

func TestSanitizeIdentifier(t *testing.T) {
	t.Parallel()

	rawName := "TestFeature/SubCase#1!"
	sanitized := nonAlphaNumRegex.ReplaceAllString(rawName, "_")
	assert.Equal(t, "TestFeature_SubCase_1_", sanitized)
}
