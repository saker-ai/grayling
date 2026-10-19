package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"github.com/saker-ai/grayling/pkg/model"
	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite"
)

const cacheFilename = "policy-cache.sqlite"

// CacheOptions uses a dedicated private directory. AuthenticationKey is an
// independently provisioned 32-byte secret, never generated or stored here.
// Minimum is an optional external anti-rollback floor (outside this directory).
// Now must be a trusted UTC clock; the default is the host clock.
type CacheOptions struct {
	kind                agentv1.ResourceKind
	bundleHash          [32]byte
	minimumMembership   uint64
	Directory, Identity string
	Scope               Scope
	Key                 string
	AuthenticationKey   []byte
	Verifier            *Verifier
	RecoveryVerifier    *RecoveryVerifier
	Minimum             Watermark
	MaxAge              time.Duration
	Now                 func() time.Time
}
type cacheFailure struct{ err error }
type cacheGuard struct {
	closed    atomic.Bool
	failure   atomic.Pointer[cacheFailure]
	verifier  atomic.Pointer[Verifier]
	revoked   atomic.Pointer[map[[32]byte]bool]
	clockHigh atomic.Int64
	now       func() time.Time
}

func (g *cacheGuard) time() (time.Time, error) {
	now := g.now().UTC()
	ms := now.UnixMilli()
	if ms <= 0 {
		return time.Time{}, ErrClockSkew
	}
	for {
		high := g.clockHigh.Load()
		if ms < high-5000 {
			return time.Time{}, ErrClockSkew
		}
		if ms <= high {
			return time.UnixMilli(high), nil
		}
		if g.clockHigh.CompareAndSwap(high, ms) {
			return now, nil
		}
	}
}

// PolicyView captures exactly one compiled policy. Normal updates do not mutate
// it. Live revocation/trust/close guards still cover previously captured views.
type PolicyView struct {
	policy     *Policy
	discovery  *Discovery
	bundle     *Bundle
	claims     Claims
	verifiedAt time.Time
	maxAge     time.Duration
	trustedKey [32]byte
	guard      *cacheGuard
}

func (v *PolicyView) Claims() Claims { return v.claims }
func (v *PolicyView) check() error {
	if v.guard.closed.Load() {
		return ErrCacheClosed
	}
	if failure := v.guard.failure.Load(); failure != nil {
		return failure.err
	}
	trust := v.guard.verifier.Load()
	if trust == nil {
		return ErrTrustChanged
	}
	root, ok := trust.keys[v.claims.KeyID]
	if !ok || !bytes.Equal(root.PublicKey, v.trustedKey[:]) || time.UnixMilli(v.claims.IssuedUnixMs).Before(root.NotBefore) || time.UnixMilli(v.claims.ExpiresUnixMs).After(root.NotAfter) {
		return ErrTrustChanged
	}
	if set := v.guard.revoked.Load(); set != nil && (*set)[v.claims.BundleHash] {
		return ErrPolicyRevoked
	}
	now, err := v.guard.time()
	if err != nil {
		return err
	}
	if now.Before(time.UnixMilli(v.claims.IssuedUnixMs)) || !now.Before(time.UnixMilli(v.claims.ExpiresUnixMs)) {
		return ErrExpired
	}
	if !now.Before(v.verifiedAt.Add(v.maxAge)) {
		if v.discovery != nil {
			return ErrDiscoveryStale
		}
		if v.bundle != nil {
			return ErrBundleStale
		}
		return ErrPolicyStale
	}
	return nil
}
func (v *PolicyView) Evaluate(key string, context model.EvaluationContext) (model.Result, error) {
	if err := v.check(); err != nil {
		return model.Result{}, err
	}
	if v.policy == nil {
		return model.Result{}, ErrCacheNotReady
	}
	return v.policy.Evaluate(key, context), nil
}

type cacheStatus struct {
	view *PolicyView
	err  error
}
type PolicyCache struct {
	db       *sql.DB
	lockFile *os.File
	unlock   func()
	opts     CacheOptions
	binding  []byte
	guard    *cacheGuard
	status   atomic.Pointer[cacheStatus]
	mu       sync.Mutex
	closeErr error
}

func cacheOptions(o CacheOptions) (CacheOptions, []byte, error) {
	if o.kind == 0 {
		o.kind = agentv1.ResourceKind_RESOURCE_KIND_POLICY
	}
	if o.kind != agentv1.ResourceKind_RESOURCE_KIND_POLICY && o.kind != agentv1.ResourceKind_RESOURCE_KIND_DISCOVERY && o.kind != agentv1.ResourceKind_RESOURCE_KIND_BUNDLE {
		return o, nil, ErrCacheCorrupt
	}
	if o.Directory == "" || !validText(o.Identity) || !validText(o.Scope.WorkspaceID) || !validText(o.Scope.EnvironmentID) || !validText(o.Scope.ServiceKey) || !validText(o.Key) || len(o.AuthenticationKey) != 32 || bytes.Equal(o.AuthenticationKey, make([]byte, 32)) || o.Verifier == nil || !independentRecoveryRoots(o.RecoveryVerifier, o.Verifier) || !validWatermark(o.Minimum) {
		return o, nil, ErrCacheCorrupt
	}
	if o.MaxAge == 0 {
		o.MaxAge = 5 * time.Minute
	}
	if o.MaxAge < time.Millisecond || o.MaxAge > 5*time.Minute {
		return o, nil, ErrCacheCorrupt
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	directory, err := filepath.Abs(o.Directory)
	if err != nil {
		return o, nil, err
	}
	o.Directory = directory
	o.AuthenticationKey = bytes.Clone(o.AuthenticationKey)
	var binding []byte
	for _, s := range []string{o.Identity, o.Scope.WorkspaceID, o.Scope.EnvironmentID, o.Scope.ServiceKey, o.Key} {
		binding = binary.BigEndian.AppendUint16(binding, uint16(len(s)))
		binding = append(binding, s...)
	}
	if o.kind == agentv1.ResourceKind_RESOURCE_KIND_DISCOVERY {
		binding = append([]byte("GDC1"), binding...)
	} else if o.kind == agentv1.ResourceKind_RESOURCE_KIND_BUNDLE {
		binding = append([]byte("GBC1"), binding...)
	}
	return o, binding, nil
}

// InitializePolicyCache is a separate explicit first-boot action. It never
// replaces an existing database, including an invalid or empty one.
func InitializePolicyCache(ctx context.Context, options CacheOptions) (*PolicyCache, error) {
	return openPolicyCache(ctx, options, true)
}

// OpenPolicyCache never creates missing state or clears a saved watermark.
// An authentic but expired/untrusted-key resource opens NOT_READY so a newer
// network resource can recover it while retaining its durable high watermark.
func OpenPolicyCache(ctx context.Context, options CacheOptions) (*PolicyCache, error) {
	return openPolicyCache(ctx, options, false)
}
func openPolicyCache(ctx context.Context, options CacheOptions, initialize bool) (*PolicyCache, error) {
	o, binding, err := cacheOptions(options)
	if err != nil {
		return nil, err
	}
	filename := cacheFilename
	if o.kind == agentv1.ResourceKind_RESOURCE_KIND_DISCOVERY {
		filename = "discovery-cache.sqlite"
	} else if o.kind == agentv1.ResourceKind_RESOURCE_KIND_BUNDLE {
		filename = "bundle-cache.sqlite"
	}
	if initialize {
		if err := os.MkdirAll(o.Directory, 0o700); err != nil {
			return nil, err
		}
	}
	info, err := os.Lstat(o.Directory)
	if err != nil {
		return nil, ErrCacheMissing
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil, ErrCacheCorrupt
	}
	root, err := os.OpenRoot(o.Directory)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	flags := os.O_RDWR
	if initialize {
		flags |= os.O_CREATE | os.O_EXCL
	} else {
		info, err = root.Lstat(filename)
		if err != nil {
			return nil, ErrCacheMissing
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return nil, ErrCacheCorrupt
		}
	}
	file, err := root.OpenFile(filename, flags, 0o600)
	if err != nil {
		return nil, err
	}
	unlock, err := lockCacheFile(file)
	if err != nil {
		file.Close()
		return nil, err
	}
	failed := true
	defer func() {
		if failed {
			unlock()
			file.Close()
		}
	}()
	settings := url.Values{"_txlock": {"immediate"}, "_pragma": {"busy_timeout(5000)", "journal_mode(WAL)", "synchronous(FULL)", "foreign_keys(1)"}}
	location := url.URL{Scheme: "file", Path: filepath.Join(o.Directory, filename), RawQuery: settings.Encode()}
	db, err := sql.Open("sqlite", location.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	defer func() {
		if failed {
			db.Close()
		}
	}()
	if err := db.PingContext(ctx); err != nil {
		return nil, err
	}
	before, err := file.Stat()
	if err != nil {
		return nil, err
	}
	after, err := root.Lstat(filename)
	if err != nil || !os.SameFile(before, after) {
		return nil, ErrCacheCorrupt
	}
	c := &PolicyCache{db: db, lockFile: file, unlock: unlock, opts: o, binding: binding, guard: &cacheGuard{now: o.Now}}
	c.guard.verifier.Store(o.Verifier)
	if initialize {
		now, err := c.guard.time()
		if err != nil {
			return nil, err
		}
		r := cacheRecord{mark: o.Minimum, clockHighMs: now.UnixMilli()}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, `CREATE TABLE agent_policy_cache(id INTEGER PRIMARY KEY CHECK(id=1),version INTEGER NOT NULL,payload BLOB NOT NULL,seal BLOB NOT NULL)`); err != nil {
			return nil, err
		}
		body, err := encodeCache(r)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO agent_policy_cache VALUES(1,1,?,?)`, body, cacheMAC(o.AuthenticationKey, binding, body)); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	r, err := c.read(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	c.restore(r)
	failed = false
	return c, nil
}
func (c *PolicyCache) read(ctx context.Context, tx *sql.Tx) (cacheRecord, error) {
	var version, size int
	var body, seal []byte
	err := tx.QueryRowContext(ctx, `SELECT version,length(payload),CASE WHEN length(payload)<=? THEN payload ELSE NULL END,seal FROM agent_policy_cache WHERE id=1`, maxCacheRecordBytes).Scan(&version, &size, &body, &seal)
	if err != nil || version != 1 || size > maxCacheRecordBytes || size != len(body) || !hmac.Equal(seal, cacheMAC(c.opts.AuthenticationKey, c.binding, body)) {
		return cacheRecord{}, ErrCacheCorrupt
	}
	r, err := decodeCache(body)
	if err != nil {
		return cacheRecord{}, err
	}
	if !aboveFloor(r.mark, c.opts.Minimum) {
		return cacheRecord{}, ErrRollback
	}
	if len(r.resource) > 0 {
		resource := new(agentv1.SignedResource)
		if err := proto.Unmarshal(r.resource, resource); err != nil {
			return cacheRecord{}, ErrCacheCorrupt
		}
		claims, err := ParseClaims(resource.Claims)
		if err != nil || claims.Scope != c.opts.Scope || claims.Kind != c.opts.kind || claims.Key != c.opts.Key {
			return cacheRecord{}, ErrCacheCorrupt
		}
		if c.opts.kind == agentv1.ResourceKind_RESOURCE_KIND_BUNDLE && (claims.BundleHash != c.opts.bundleHash || r.mark.BundleHash != c.opts.bundleHash || sha256.Sum256(resource.Payload) != c.opts.bundleHash) {
			return cacheRecord{}, ErrCacheCorrupt
		}
	}
	if err := c.checkDiscoveryRecord(r); err != nil {
		return cacheRecord{}, err
	}
	return r, nil
}
func (c *PolicyCache) write(ctx context.Context, tx *sql.Tx, r cacheRecord) error {
	body, err := encodeCache(r)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE agent_policy_cache SET payload=?,seal=? WHERE id=1`, body, cacheMAC(c.opts.AuthenticationKey, c.binding, body))
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		c.poison(ErrCacheIO)
		return ErrCacheIO
	}
	if err := tx.Commit(); err != nil {
		c.poison(ErrCacheIO)
		return errors.Join(ErrCacheIO, err)
	}
	return nil
}
func (c *PolicyCache) poison(err error) {
	c.guard.failure.Store(&cacheFailure{err: err})
	c.status.Store(&cacheStatus{err: err})
}
func (c *PolicyCache) restore(r cacheRecord) {
	for {
		previous := c.guard.clockHigh.Load()
		if previous >= r.clockHighMs || c.guard.clockHigh.CompareAndSwap(previous, r.clockHighMs) {
			break
		}
	}
	set := revokedSet(r.revoked)
	c.guard.revoked.Store(&set)
	now, err := c.guard.time()
	if err != nil {
		c.status.Store(&cacheStatus{err: err})
		return
	}
	if len(r.resource) == 0 {
		c.status.Store(&cacheStatus{err: ErrCacheNotReady})
		return
	}
	resource := new(agentv1.SignedResource)
	if err := proto.Unmarshal(r.resource, resource); err != nil {
		c.poison(ErrCacheCorrupt)
		return
	}
	verified, err := c.verifyResource(resource, r.mark, now)
	if err != nil {
		c.status.Store(&cacheStatus{err: err})
		return
	}
	claims := verified.claims
	view := &PolicyView{policy: verified.policy, discovery: verified.discovery, bundle: verified.bundle, claims: claims, verifiedAt: time.UnixMilli(r.verifiedMs), maxAge: c.opts.MaxAge, trustedKey: publicKeyBytes(c.opts.Verifier, claims.KeyID), guard: c.guard}
	if err := view.check(); err != nil {
		c.status.Store(&cacheStatus{err: err})
		return
	}
	c.status.Store(&cacheStatus{view: view})
}
func (c *PolicyCache) Capture() (*PolicyView, error) {
	if c.guard.closed.Load() {
		return nil, ErrCacheClosed
	}
	s := c.status.Load()
	if s == nil {
		return nil, ErrCacheNotReady
	}
	if s.err != nil {
		return nil, s.err
	}
	if err := s.view.check(); err != nil {
		return nil, err
	}
	return s.view, nil
}
func (c *PolicyCache) Watermark(ctx context.Context) (Watermark, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.guard.closed.Load() {
		return Watermark{}, ErrCacheClosed
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return Watermark{}, err
	}
	defer tx.Rollback()
	r, err := c.read(ctx, tx)
	if err != nil {
		c.poison(err)
		return Watermark{}, err
	}
	return r.mark, tx.Commit()
}

// Install accepts a complete resource obtained through an authenticated sync.
// Only this path refreshes policy freshness; cache reads and link heartbeats do
// not. A candidate failure preserves the prior valid in-memory policy.
func (c *PolicyCache) Install(ctx context.Context, resource *agentv1.SignedResource) error {
	return c.install(ctx, resource, nil)
}

// InstallRecovery requires a separately trusted grant bound to the current durable
// watermark and exact candidate. It advances epoch without clearing history.
func (c *PolicyCache) InstallRecovery(ctx context.Context, resource *agentv1.SignedResource, grant *RecoveryGrant) error {
	if grant == nil || len(grant.Claims) > maxRecoveryBytes || len(grant.Signature) != ed25519.SignatureSize {
		return ErrResource
	}
	grant = &RecoveryGrant{Claims: bytes.Clone(grant.Claims), Signature: bytes.Clone(grant.Signature)}
	return c.install(ctx, resource, grant)
}
func (c *PolicyCache) install(ctx context.Context, resource *agentv1.SignedResource, grant *RecoveryGrant) error {
	if grant != nil && c.opts.kind == agentv1.ResourceKind_RESOURCE_KIND_BUNDLE {
		return ErrResource
	}
	if resource == nil || proto.Size(resource) > MaxResourceBytes {
		return ErrResource
	}
	resource = proto.Clone(resource).(*agentv1.SignedResource)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.guard.closed.Load() {
		return ErrCacheClosed
	}
	if fail := c.guard.failure.Load(); fail != nil {
		return fail.err
	}
	now, err := c.guard.time()
	if err != nil {
		return err
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := c.read(ctx, tx)
	if err != nil {
		c.poison(err)
		return err
	}
	mark := r.mark
	var recoveryDigest [32]byte
	replay := false
	if grant != nil {
		authorization, digest, err := c.opts.RecoveryVerifier.verify(grant, c.opts.Scope, c.opts.Key, now)
		if err != nil {
			return err
		}
		candidate, err := ParseClaims(resource.Claims)
		if err != nil {
			return err
		}
		if candidate != authorization.Target {
			return ErrResource
		}
		replay = r.mark == authorization.Target.Watermark() && r.recoveryDigest == digest
		if !replay && authorization.From != r.mark {
			return ErrRollback
		}
		mark = authorization.Target.Watermark()
		recoveryDigest = digest
	}
	verified, err := c.verifyResource(resource, mark, now)
	if err != nil {
		return err
	}
	claims := verified.claims
	if err := c.checkDiscoveryUpdate(r, resource, claims); err != nil {
		return err
	}
	if revokedSet(r.revoked)[claims.BundleHash] {
		return ErrPolicyRevoked
	}
	if c.opts.kind == agentv1.ResourceKind_RESOURCE_KIND_BUNDLE && len(r.resource) > 0 && claims.Watermark() == r.mark {
		encoded, err := proto.Marshal(resource)
		if err != nil {
			return ErrResource
		}
		if !bytes.Equal(encoded, r.resource) {
			return ErrRollback
		}
		return tx.Commit()
	}
	if replay || c.opts.kind == agentv1.ResourceKind_RESOURCE_KIND_DISCOVERY && len(r.resource) > 0 && claims.Watermark() == r.mark {
		return tx.Commit()
	}
	body, err := proto.Marshal(resource)
	if err != nil {
		return ErrResource
	}
	r.mark = claims.Watermark()
	if grant != nil {
		r.recoveryDigest = recoveryDigest
	}
	r.resource = body
	r.verifiedMs = now.UnixMilli()
	r.clockHighMs = c.guard.clockHigh.Load()
	if err := c.write(ctx, tx, r); err != nil {
		return err
	}
	// Do not expose a candidate before its resource and watermark commit together.
	view := &PolicyView{policy: verified.policy, discovery: verified.discovery, bundle: verified.bundle, claims: claims, verifiedAt: now, maxAge: c.opts.MaxAge, trustedKey: publicKeyBytes(c.opts.Verifier, claims.KeyID), guard: c.guard}
	if err := view.check(); err != nil {
		c.status.Store(&cacheStatus{err: err})
		return err
	}
	c.status.Store(&cacheStatus{view: view})
	return nil
}

// SetVerifier changes the trust ring. Previously captured views stop accepting
// new evaluations only if their signer is no longer trusted; overlapping keys
// keep previously captured views valid. The current policy is rebuilt without
// renewing its freshness.
func (c *PolicyCache) SetVerifier(ctx context.Context, v *Verifier) error {
	if v == nil || !independentRecoveryRoots(c.opts.RecoveryVerifier, v) {
		return ErrResource
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.guard.closed.Load() {
		return ErrCacheClosed
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := c.read(ctx, tx)
	if err != nil {
		c.poison(err)
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	c.opts.Verifier = v
	c.guard.verifier.Store(v)
	c.restore(r)
	return nil
}
func (c *PolicyCache) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.guard.closed.Swap(true) {
		return c.closeErr
	}
	// Persist the largest observed clock without extending resource freshness.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := c.db.BeginTx(ctx, nil)
	if err == nil {
		var r cacheRecord
		r, err = c.read(ctx, tx)
		if err == nil {
			r.clockHighMs = max(r.clockHighMs, c.guard.clockHigh.Load())
			err = c.write(ctx, tx, r)
		}
		_ = tx.Rollback()
	}
	c.closeErr = errors.Join(err, c.db.Close())
	c.unlock()
	c.closeErr = errors.Join(c.closeErr, c.lockFile.Close())
	clear(c.opts.AuthenticationKey)
	return c.closeErr
}

func publicKeyBytes(v *Verifier, id string) [32]byte {
	var key [32]byte
	if v != nil {
		copy(key[:], v.keys[id].PublicKey)
	}
	return key
}
