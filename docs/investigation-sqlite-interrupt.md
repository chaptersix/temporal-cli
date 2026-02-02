# SQLite Interrupt Investigation: "Namespace default is not found"

## Customer Issue Summary

**Ticket**: ACT-718
**Customer**: Mercury (Joe Kachmar, Andrew Smith)
**Impact**: 3.3% of CI runs fail, requires full CI re-run

### Symptoms

1. Error message: `"Namespace default was not found or otherwise could not be described"`
2. gRPC status code: `Unavailable`
3. gRPC error message: `"GetNamespace operation failed. Error interrupted (9)"`
4. **Critical**: Once error occurs, ALL subsequent requests fail until server restart
5. Happens with both in-memory SQLite and file-based SQLite (less frequent with file-based)

### Customer's Setup

- Using `temporal server start-dev`
- Rust SDK via FFI
- Call chain: `test suite → startWorker → validateWorker → sdk-core (Rust FFI) → validate → verify_namespace_exists`
- They have a delay between server start and tests
- They wait for dev server logs to indicate default namespace worker is present
- Added readiness probe: `temporal operator namespace describe --namespace default`

### Key Observation from Customer

> "when we see this error in testing it happens for **all tests run after the dev server has started up**, so individual tests run against the dev server will fail **until the server is restarted**"

This means it's NOT a transient error - once it happens, the server is in a broken state.

---

## Technical Analysis

### Error Code Breakdown

The `(9)` in the error is **NOT** a gRPC code. It's `SQLITE_INTERRUPT`:
- SQLite error code 9 = `SQLITE_INTERRUPT`
- Means: "Operation terminated by sqlite3_interrupt()"

### Code Path

1. **Error surfaces at**: `vendor/go.temporal.io/server/common/persistence/sql/metadata.go:104`
   ```go
   return nil, serviceerror.NewUnavailablef("GetNamespace operation failed. Error %v", err)
   ```

2. **SQLite query**: `metadata.go:92`
   ```go
   rows, err := m.DB.SelectFromNamespace(ctx, filter)
   ```

3. **Interrupt triggered by**: `vendor/modernc.org/sqlite/sqlite.go:801-820`
   ```go
   func interruptOnDone(ctx context.Context, c *conn, done *int32) func() {
       // ...
       case <-ctx.Done():
           c.interrupt(c.db)  // Calls sqlite3_interrupt()
   ```

4. **Connection marked bad**: `vendor/modernc.org/sqlite/sqlite.go:1559-1575`
   ```go
   func (c *conn) ResetSession(ctx context.Context) error {
       if !c.usable() {
           return driver.ErrBadConn
       }
       return nil
   }

   func (c *conn) usable() bool {
       return c.db != 0 && sqlite3.Xsqlite3_is_interrupted(c.tls, c.db) == 0
   }
   ```

### SQLite Connection Configuration

**File**: `vendor/go.temporal.io/server/common/persistence/sql/sqlplugin/sqlite/plugin.go:90-99`

```go
// Dealing with the error `database is locked`
// > ... set the database connections of the SQL package to 1.
db.SetMaxOpenConns(1)

// Settings for in-memory database (should be fine for file mode as well)
// > Note that if the last database connection in the pool closes, the in-memory database is deleted.
// > Make sure the max idle connection limit is > 0, and the connection lifetime is infinite.
db.SetMaxIdleConns(1)
db.SetConnMaxIdleTime(0)
```

### Namespace Registry Timeout

**File**: `vendor/go.temporal.io/server/common/namespace/nsregistry/registry.go:32-33`

```go
readthroughCacheTTL              = 1 * time.Second
readthroughTimeout               = 3 * time.Second  // Context timeout for namespace queries
```

### Error Masking Issue

**File**: `vendor/go.temporal.io/server/common/namespace/nsregistry/registry.go:559-560`

```go
// TODO: we should return the actual error we got (e.g. timeout)
return nil, serviceerror.NewNamespaceNotFound(name.String())
```

The actual SQLite error gets masked as "NamespaceNotFound" in some code paths.

---

## Key Files

| File | Purpose |
|------|---------|
| `vendor/go.temporal.io/server/common/persistence/sql/metadata.go` | Where GetNamespace error is wrapped |
| `vendor/go.temporal.io/server/common/persistence/sql/sqlplugin/sqlite/plugin.go` | SQLite connection config |
| `vendor/go.temporal.io/server/common/persistence/sql/sqlplugin/sqlite/conn_pool.go` | Connection pool management |
| `vendor/go.temporal.io/server/common/namespace/nsregistry/registry.go` | Namespace registry with timeouts |
| `vendor/modernc.org/sqlite/sqlite.go` | SQLite driver - interrupt handling |
| `internal/devserver/server.go` | Dev server startup |
| `temporalcli/commands.sqlite_interrupt_test.go` | Bug reproduction test |

---

## Root Cause: CONFIRMED

### Bug in modernc.org/sqlite v1.34.1

**Customer CLI version**: v1.5.0 uses `modernc.org/sqlite v1.34.1`

**GitLab Issue**: https://gitlab.com/cznic/sqlite/-/issues/196

**Fix Commit**: https://gitlab.com/cznic/sqlite/-/commit/d33e5682

**Fix Version**: v1.40.1 (`close_rows_on_canceled_context` branch merged)

**Related Issues** (same bug in other drivers):
- [mattn/go-sqlite3 #745](https://github.com/mattn/go-sqlite3/issues/745) - Racy context cancellation
- [mattn/go-sqlite3 PR #744](https://github.com/mattn/go-sqlite3/pull/744) - Fix for CGo driver

### The Bug

In v1.34.1, when a context is canceled during a query, rows are **NOT explicitly closed**:

```go
// v1.34.1 - sqlite.go, around line 620
defer func() {
    if pstmt != 0 {
        e := s.c.finalize(pstmt)
        // ...
    }
    if ctx != nil && atomic.LoadInt32(&done) != 0 {
        r, err = nil, ctx.Err()  // Rows NOT closed!
    }
}()
```

In v1.40.1, this is fixed:

```go
// v1.40.1 - sqlite.go, around line 647
defer func() {
    if ctx != nil && atomic.LoadInt32(&done) != 0 {
        if r != nil {
            r.Close()  // <-- FIX: Explicitly close rows
        }
        r, err = nil, ctx.Err()
    }
    // ... finalize after
}()
```

### Why This Causes Persistent Failure

Per [SQLite docs](https://sqlite.org/c3ref/interrupt.html):
> "The interrupt flag is reset after all currently running SQL statements complete."

When rows aren't closed:
1. Context times out (3 second `readthroughTimeout` in namespace registry)
2. `interruptOnDone()` goroutine calls `sqlite3_interrupt()`
3. **BUG**: Rows are not closed, statement not finalized
4. SQLite still considers the statement "running"
5. Interrupt flag remains set (running statement count > 0)
6. All subsequent `sqlite3_step()` calls return `SQLITE_INTERRUPT (9)`
7. Server is in broken state until restart

### Version Comparison

| Version | `ResetSession` | `IsValid` | Context Cancel Rows |
|---------|----------------|-----------|---------------------|
| v1.34.1 | `c.db == 0` only | `c.db != 0` only | NOT closed |
| v1.40.1 | `usable()` (checks interrupt) | `usable()` | **Explicitly closed** |

### Why 3.3% Failure Rate

The bug requires a specific race condition:
1. A query must be in-flight when context times out
2. The 3-second timeout must expire during a slow operation
3. CI resource constraints make this more likely (slow disk, CPU contention)

---

## Previous Hypotheses (Superseded)

### Hypothesis 1: Interrupt Flag Persists (CONFIRMED - see Root Cause)
- `sqlite3_interrupt()` sets a flag
- Flag should clear when active statement count reaches zero
- **ROOT CAUSE**: Statement count never reaches zero because rows aren't closed
- All subsequent queries return `SQLITE_INTERRUPT`

### Hypothesis 2: Connection Discarded, Database Lost (RULED OUT)
- Not relevant - the issue is the interrupt flag, not connection loss

### Hypothesis 3: Specific Timing/Environment Condition (CONFIRMED)
- 3.3% failure rate = race condition during context timeout
- CI resource constraints make timeout more likely to hit during query

---

## Bug Reproduction

### Reproduction Test

**File**: `temporalcli/commands.sqlite_interrupt_test.go`

The test reproduces the bug using the standard SQLite driver API (no vendor modifications required):

1. Registers a custom SQL function that blocks until context is canceled
2. Runs a query with a timeout shorter than the blocking function
3. Context times out, triggering `sqlite3_interrupt()`
4. Rows are NOT closed (the bug in v1.34.1)
5. All subsequent queries fail with `interrupted (9)`

### Test Output (v1.34.1 - BUG PRESENT)

```
=== RUN   TestSQLiteInterruptBug
    Starting blocking query...
    Releasing blocking function...
    Query returned error: context deadline exceeded
    First query result: err=context deadline exceeded
    Issuing subsequent queries...
      Query 1: FAILED - interrupted (9)
      Query 2: FAILED - interrupted (9)
      Query 3: FAILED - interrupted (9)
      Query 4: FAILED - interrupted (9)
      Query 5: FAILED - interrupted (9)
    Results: 0 successes, 5 failures
    WAL checkpoint FAILED: interrupted (9) (indicates leaked rows)
    BUG REPRODUCED: 5 subsequent queries failed after context cancellation
--- PASS: TestSQLiteInterruptBug (0.70s)
```

### Run the Test

```bash
go test -v -run TestSQLiteInterruptBug ./temporalcli/ -count=1
```

### Key Findings

1. First query returns `context deadline exceeded` (expected)
2. **ALL subsequent queries fail with `interrupted (9)`** (the bug)
3. WAL checkpoint also fails (confirms rows leaked in unclosed state)
4. Server is permanently broken until restart

---

## Workarounds for Customer

1. **Retry on DescribeNamespace** before starting workers
2. **Add delay** between server start and test execution (they already do this)
3. **Readiness probe**: `temporal operator namespace describe --namespace default` with retries (they added this)
4. **If error occurs mid-run**: Restart dev server and retry failed tests

---

## Recommended Fix

### Upgrade modernc.org/sqlite to v1.40.1+

**In go.mod**:
```
modernc.org/sqlite v1.40.1
```

This version includes the `close_rows_on_canceled_context` fix that properly closes rows when context is canceled, allowing the interrupt flag to clear.

### Alternative/Additional Fixes (In Temporal Server)

1. **Don't mask errors**: Fix TODO at `registry.go:559-560` to return actual error
2. **Retry transient errors**: Add retry logic in `getNamespaceByNamePersistence()`
3. **Increase timeout**: The 3-second `readthroughTimeout` may be too short under CI load

---

## Commands for Investigation

```bash
# Run bug reproduction test (requires CLI v1.5.0 with sqlite v1.34.1)
go test -v -run TestSQLiteInterruptBug ./temporalcli/ -count=1
```

---

## Vendor Code Locations (After `go mod vendor`)

```
vendor/go.temporal.io/server/common/persistence/sql/metadata.go
vendor/go.temporal.io/server/common/persistence/sql/sqlplugin/sqlite/plugin.go
vendor/go.temporal.io/server/common/persistence/sql/sqlplugin/sqlite/conn_pool.go
vendor/go.temporal.io/server/common/namespace/nsregistry/registry.go
vendor/modernc.org/sqlite/sqlite.go
```
