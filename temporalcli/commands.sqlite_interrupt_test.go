package temporalcli_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"modernc.org/sqlite"
)

// blockCtx is used to control the blocking function
var blockCtx context.Context
var blockCancel context.CancelFunc

func init() {
	// Register blocking function at init time so it's available for all connections
	sqlite.MustRegisterDeterministicScalarFunction(
		"test_block",
		0,
		func(fc *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			if blockCtx != nil {
				<-blockCtx.Done()
			}
			return int64(42), nil
		},
	)
}

// TestSQLiteInterruptBug tests the SQLite interrupt bug in v1.34.1
// where rows not being closed on context cancellation causes the
// interrupt flag to persist, breaking all subsequent queries.
//
// This test uses a custom SQL function that blocks until context is canceled,
// similar to the test in modernc.org/sqlite v1.40.1.
//
// To run this test:
//
//	go test -v -run TestSQLiteInterruptBug ./temporalcli/ -count=1
func TestSQLiteInterruptBug(t *testing.T) {
	// Set up blocking context
	blockCtx, blockCancel = context.WithCancel(context.Background())

	// Open an in-memory database with a single connection (like Temporal dev server)
	db, err := sql.Open("sqlite", ":memory:?cache=shared")
	require.NoError(t, err)
	defer db.Close()

	// Configure like Temporal dev server
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxIdleTime(0)

	// Create a test table
	_, err = db.Exec("CREATE TABLE test (id INTEGER PRIMARY KEY, name TEXT)")
	require.NoError(t, err)
	_, err = db.Exec("INSERT INTO test (name) VALUES ('test1'), ('test2'), ('test3')")
	require.NoError(t, err)

	// Get a single connection to use
	conn, err := db.Conn(context.Background())
	require.NoError(t, err)
	defer conn.Close()

	// Create a context for the query that will time out
	queryCtx, queryCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer queryCancel()

	// Run a query that will block, then cancel the context
	var wg sync.WaitGroup
	wg.Add(1)

	var queryErr error
	go func() {
		defer wg.Done()
		t.Log("Starting blocking query...")
		rows, err := conn.QueryContext(queryCtx, "SELECT test_block()")
		if err != nil {
			t.Logf("Query returned error: %v", err)
			queryErr = err
			return
		}
		// If we get here, the query succeeded (shouldn't happen with timeout)
		t.Log("Query succeeded unexpectedly, closing rows")
		rows.Close()
	}()

	// Wait a bit for the query to start and context to timeout
	time.Sleep(700 * time.Millisecond)
	t.Log("Releasing blocking function...")
	blockCancel()
	wg.Wait()

	t.Logf("First query result: err=%v", queryErr)

	// Now try subsequent queries - these should fail if the bug is present
	t.Log("Issuing subsequent queries...")

	var successes, failures int
	for i := 0; i < 5; i++ {
		rows, err := conn.QueryContext(context.Background(), "SELECT * FROM test")
		if err != nil {
			t.Logf("  Query %d: FAILED - %v", i+1, err)
			failures++
			continue
		}

		count := 0
		for rows.Next() {
			count++
		}
		if err := rows.Err(); err != nil {
			t.Logf("  Query %d: FAILED during iteration - %v", i+1, err)
			failures++
		} else {
			t.Logf("  Query %d: SUCCESS - %d rows", i+1, count)
			successes++
		}
		rows.Close()
	}

	t.Logf("Results: %d successes, %d failures", successes, failures)

	// Verify with WAL checkpoint (this fails if rows leaked)
	t.Log("Running WAL checkpoint...")
	_, err = conn.ExecContext(context.Background(), "PRAGMA wal_checkpoint")
	if err != nil {
		t.Logf("WAL checkpoint FAILED: %v (indicates leaked rows)", err)
	} else {
		t.Log("WAL checkpoint succeeded")
	}

	if failures > 0 {
		t.Logf("BUG REPRODUCED: %d subsequent queries failed after context cancellation", failures)
	} else {
		t.Log("No failures observed - bug may be fixed in this version")
	}
}
