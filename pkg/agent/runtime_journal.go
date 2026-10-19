package agent

import (
	"bytes"
	"context"
	"crypto/hmac"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
)

const runtimeCacheFilename = "runtime-cache.sqlite"

type runtimeJournal struct {
	db      *sql.DB
	file    *os.File
	unlock  func()
	options RuntimeOptions
	binding []byte
}

func openRuntimeJournal(ctx context.Context, o RuntimeOptions, initialize bool) (*runtimeJournal, runtimeRecord, error) {
	var empty runtimeRecord
	base, binding, err := cacheOptions(CacheOptions{Directory: o.Directory, Identity: o.Identity, Scope: o.Scope, Key: o.PolicyKey, AuthenticationKey: o.AuthenticationKey, Verifier: o.Verifier, MaxAge: o.MaxAge, Now: o.Now})
	if err != nil {
		return nil, empty, err
	}
	o.Directory = base.Directory
	o.AuthenticationKey = base.AuthenticationKey
	o.MaxAge = base.MaxAge
	binding = append([]byte("runtime\x00"), binding...)
	binding = append(binding, []byte(o.InstanceID)...)
	if initialize {
		if err := os.MkdirAll(o.Directory, 0700); err != nil {
			return nil, empty, err
		}
	}
	info, err := os.Lstat(o.Directory)
	if err != nil {
		return nil, empty, ErrCacheMissing
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, empty, ErrCacheCorrupt
	}
	root, err := os.OpenRoot(o.Directory)
	if err != nil {
		return nil, empty, err
	}
	defer root.Close()
	flags := os.O_RDWR
	if initialize {
		flags |= os.O_CREATE | os.O_EXCL
	} else {
		info, err = root.Lstat(runtimeCacheFilename)
		if err != nil {
			return nil, empty, ErrCacheMissing
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return nil, empty, ErrCacheCorrupt
		}
	}
	file, err := root.OpenFile(runtimeCacheFilename, flags, 0600)
	if err != nil {
		return nil, empty, err
	}
	unlock, err := lockCacheFile(file)
	if err != nil {
		file.Close()
		return nil, empty, err
	}
	failed := true
	defer func() {
		if failed {
			unlock()
			file.Close()
		}
	}()
	settings := url.Values{"_txlock": {"immediate"}, "_pragma": {"busy_timeout(5000)", "journal_mode(WAL)", "synchronous(FULL)", "foreign_keys(1)"}}
	location := url.URL{Scheme: "file", Path: filepath.Join(o.Directory, runtimeCacheFilename), RawQuery: settings.Encode()}
	db, err := sql.Open("sqlite", location.String())
	if err != nil {
		return nil, empty, err
	}
	defer func() {
		if failed {
			db.Close()
		}
	}()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		return nil, empty, err
	}
	before, err := file.Stat()
	if err != nil {
		return nil, empty, err
	}
	after, err := root.Lstat(runtimeCacheFilename)
	if err != nil || !os.SameFile(before, after) {
		return nil, empty, ErrCacheCorrupt
	}
	j := &runtimeJournal{db: db, file: file, unlock: unlock, options: o, binding: binding}
	if initialize {
		empty = runtimeRecord{ClockHighMs: o.Now().UnixMilli(), Revoked: [][32]byte{}}
		if empty.ClockHighMs <= 0 {
			return nil, empty, ErrClockSkew
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return nil, empty, err
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, `CREATE TABLE agent_runtime_cache(id INTEGER PRIMARY KEY CHECK(id=1),version INTEGER NOT NULL,payload BLOB NOT NULL,seal BLOB NOT NULL)`); err != nil {
			return nil, empty, err
		}
		body, err := encodeRuntime(empty)
		if err != nil {
			return nil, empty, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO agent_runtime_cache VALUES(1,1,?,?)`, body, runtimeMAC(o.AuthenticationKey, binding, body)); err != nil {
			return nil, empty, err
		}
		if err := tx.Commit(); err != nil {
			return nil, empty, err
		}
	}
	record, err := j.read(ctx)
	if err != nil {
		return nil, empty, err
	}
	failed = false
	return j, record, nil
}
func (j *runtimeJournal) read(ctx context.Context) (runtimeRecord, error) {
	var version, size int
	var body, seal []byte
	err := j.db.QueryRowContext(ctx, `SELECT version,length(payload),CASE WHEN length(payload)<=? THEN payload ELSE NULL END,seal FROM agent_runtime_cache WHERE id=1`, maxRuntimeRecordBytes).Scan(&version, &size, &body, &seal)
	if err != nil {
		if ctx.Err() != nil {
			return runtimeRecord{}, ctx.Err()
		}
		if errors.Is(err, sql.ErrNoRows) {
			return runtimeRecord{}, ErrCacheCorrupt
		}
		return runtimeRecord{}, errors.Join(ErrCacheIO, err)
	}
	if version != 1 || size > maxRuntimeRecordBytes || size != len(body) || !hmac.Equal(seal, runtimeMAC(j.options.AuthenticationKey, j.binding, body)) {
		return runtimeRecord{}, ErrCacheCorrupt
	}
	r, err := decodeRuntime(body)
	if err != nil {
		return r, err
	}
	if (r.Mark != (RuntimeWatermark{}) || r.RevocationEpoch != 0) && !revocationAbove(r, j.options.MinimumRevocation) {
		return r, ErrRollback
	}
	if r.Mark != (RuntimeWatermark{}) && !runtimeAbove(r.Mark, j.options.Minimum) {
		return r, ErrRollback
	}
	return r, nil
}
func (j *runtimeJournal) write(ctx context.Context, r runtimeRecord) error {
	body, err := encodeRuntime(r)
	if err != nil {
		return err
	}
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE agent_runtime_cache SET payload=?,seal=? WHERE id=1`, body, runtimeMAC(j.options.AuthenticationKey, j.binding, body))
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return ErrCacheIO
	}
	if err := tx.Commit(); err != nil {
		return errors.Join(ErrCacheIO, err)
	}
	return nil
}
func (j *runtimeJournal) close() error {
	err := j.db.Close()
	j.unlock()
	return errors.Join(err, j.file.Close())
}
func equalRuntimeInputs(a, b [3][]byte) bool {
	for i := range 3 {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}
