// Package pgtest provides a real PostgreSQL server for tests (no mocks, no
// interface-level substitutes). Resolution order:
//
//  1. GRAYLING_TEST_PG_DSN — an admin DSN (any reachable PostgreSQL ≥ 14); a
//     unique scratch database is created per test and dropped on cleanup.
//  2. Docker — a throwaway postgres:16-alpine container bound to loopback
//     (image override: GRAYLING_TEST_PG_IMAGE).
//  3. Neither available — tests skip with an explicit reason.
package pgtest

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

var dbCounter atomic.Int64

// DSN returns a connection string to a fresh, empty PostgreSQL database and
// registers teardown. It blocks until the server accepts SQL connections.
func DSN(t *testing.T) string {
	t.Helper()
	if os.Getenv("GRAYLING_TEST_PG_SKIP") == "1" {
		t.Skip("pgtest: PostgreSQL integration tests are disabled for this job")
	}
	if admin := os.Getenv("GRAYLING_TEST_PG_DSN"); admin != "" {
		return scratchDatabase(t, admin)
	}
	return dockerPostgres(t)
}

// scratchDatabase creates (and later drops) a unique database on an
// externally provided server.
func scratchDatabase(t *testing.T, adminDSN string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	name := fmt.Sprintf("grayling_pgtest_%d_%d", os.Getpid(), dbCounter.Add(1))
	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("pgtest: connect to GRAYLING_TEST_PG_DSN: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("pgtest: create database %s: %v", name, err)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		if c, err := pgx.Connect(cctx, adminDSN); err == nil {
			defer c.Close(cctx)
			_, _ = c.Exec(cctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		}
	})
	return rewriteDB(adminDSN, name)
}

func rewriteDB(dsn, dbname string) string {
	// pgx accepts both URL and keyword DSNs; normalize the database part.
	if i := strings.LastIndex(dsn, "/"); i >= 0 && strings.HasPrefix(dsn, "postgres") {
		base := dsn[:i+1]
		rest := dsn[i+1:]
		if q := strings.Index(rest, "?"); q >= 0 {
			return base + dbname + rest[q:]
		}
		return base + dbname
	}
	return dsn + " dbname=" + dbname
}

// dockerPostgres runs a throwaway container on a free loopback port.
func dockerPostgres(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("pgtest: no docker and no GRAYLING_TEST_PG_DSN — real-PostgreSQL tests skipped: %v", err)
	}
	probe := exec.Command("docker", "info")
	probe.Stdout, probe.Stderr = nil, nil
	if err := probe.Run(); err != nil {
		t.Skipf("pgtest: docker daemon unavailable and no GRAYLING_TEST_PG_DSN — real-PostgreSQL tests skipped: %v", err)
	}
	image := os.Getenv("GRAYLING_TEST_PG_IMAGE")
	if image == "" {
		image = "postgres:16-alpine"
	}
	port := freePort(t)
	name := fmt.Sprintf("grayling-pg-%d-%d", os.Getpid(), dbCounter.Add(1))
	run := exec.Command("docker", "run", "-d", "--rm",
		"--name", name,
		"-e", "POSTGRES_PASSWORD=grayling-test",
		"-e", "POSTGRES_DB=grayling_test",
		"-p", fmt.Sprintf("127.0.0.1:%d:5432", port),
		image)
	if out, err := run.CombinedOutput(); err != nil {
		t.Skipf("pgtest: cannot run %s (docker pull/start failed) and no GRAYLING_TEST_PG_DSN — real-PostgreSQL tests skipped: %v\n%s", image, err, out)
	}
	t.Cleanup(func() {
		c := exec.Command("docker", "rm", "-f", name)
		_, _ = c.CombinedOutput()
	})
	dsn := fmt.Sprintf("postgres://postgres:grayling-test@127.0.0.1:%d/grayling_test?sslmode=disable", port)
	waitReady(t, dsn)
	return dsn
}

// waitReady blocks until the server answers a real SQL query (TCP + auth),
// which only happens after the entrypoint's initdb phase completed.
func waitReady(t *testing.T, dsn string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		conn, err := pgx.Connect(ctx, dsn)
		if err == nil {
			err = conn.Ping(ctx)
			conn.Close(ctx)
		}
		cancel()
		if err == nil {
			return
		}
		lastErr = err
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("pgtest: postgres never became ready: %v", lastErr)
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pgtest: free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// Query runs a read query against dsn and returns the rows as string
// slices (test assertions on raw table state).
func Query(t *testing.T, dsn, sql string, args ...any) [][]string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pgtest: connect: %v", err)
	}
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, sql, args...)
	if err != nil {
		t.Fatalf("pgtest: query %q: %v", sql, err)
	}
	defer rows.Close()
	var out [][]string
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			t.Fatalf("pgtest: row values: %v", err)
		}
		line := make([]string, len(vals))
		for i, v := range vals {
			line[i] = fmt.Sprint(v)
		}
		out = append(out, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("pgtest: rows: %v", err)
	}
	return out
}

// Exec runs a statement against dsn (test fixtures, e.g. backdating rows).
func Exec(t *testing.T, dsn, sql string, args ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pgtest: connect: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("pgtest: exec %q: %v", sql, err)
	}
}
