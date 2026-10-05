package platform

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Common errors returned by the store.
var (
	ErrNotFound      = errors.New("platform: not found")
	ErrExists        = errors.New("platform: already exists")
	ErrConflict      = errors.New("platform: revision conflict")
	ErrInvalidState  = errors.New("platform: invalid state transition")
	ErrAccessDenied  = errors.New("platform: access denied")
	ErrQuotaExceeded = errors.New("platform: quota exceeded")
	ErrValidation    = errors.New("platform: validation failed")
	ErrSelfApproval  = errors.New("platform: submitter cannot approve own high-risk change")
)

// Data is the serializable store state.
type Data struct {
	SchemaVersion int `json:"schemaVersion"`

	NextCounter int64 `json:"nextCounter"`
	// ControlVersion is a PostgreSQL compare-and-swap epoch for management writes.
	ControlVersion int64 `json:"controlVersion,omitempty"`

	Workspaces   map[string]*Workspace   `json:"workspaces"`   // by ID
	EnvBySlugKey map[string]string       `json:"envBySlugKey"` // wsID/slug -> envID
	Environments map[string]*Environment `json:"environments"` // by ID

	Tokens       map[string]*Token `json:"tokens"`       // by token ID
	TokensByHash map[string]string `json:"tokensByHash"` // secret hash -> token ID

	FlagDrafts     map[string]*FlagDraft      `json:"flagDrafts"`     // ws/env/key
	FlagRevisions  map[string][]*FlagRevision `json:"flagRevisions"`  // ws/env/key, ascending
	CurrentFlagRev map[string]int64           `json:"currentFlagRev"` // ws/env/key -> revision in force
	NextFlagRev    map[string]int64           `json:"nextFlagRev"`
	FlagTombstones map[string]int64           `json:"flagTombstones"` // ws/env/key -> manifest rev of deletion

	SegmentDrafts     map[string]*SegmentDraft      `json:"segmentDrafts"`
	SegmentRevisions  map[string][]*SegmentRevision `json:"segmentRevisions"`
	CurrentSegmentRev map[string]int64              `json:"currentSegmentRev"`
	NextSegmentRev    map[string]int64              `json:"nextSegmentRev"`

	ManifestRevisions map[string]int64 `json:"manifestRevisions"` // ws/env -> last published manifest revision

	Experiments map[string]*Experiment `json:"experiments"` // by ID

	Events       []*Event                   `json:"events"`
	EventIndex   map[string]int             `json:"eventIndex"`   // eventID -> index into Events
	UnitAggs     map[string]*UnitAgg        `json:"unitAggs"`     // aggKey -> stats
	ExposedUnits map[string]map[string]bool `json:"exposedUnits"` // ws/env/exp/epoch/variant -> unitHash set
	// SplitExposedUnits tracks only exposures whose Source == "split" — the
	// randomized population. SRM expectations are pre-registered
	// proportions, so SRM must be computed on this set alone; targeting /
	// default arms are deterministic and would bias the test (§10.1).
	SplitExposedUnits map[string]map[string]bool `json:"splitExposedUnits"`
	EventCounts       map[string]int64           `json:"eventCounts"` // ws/env/exp/epoch/variant/exposure|conversion -> count

	Audit     []*AuditEntry `json:"audit"`
	NextAudit int64         `json:"nextAudit"`

	AIResources    map[string]*AIResourceRef    `json:"aiResources"`    // ws/env/type/logical
	PinnedVersions map[string][]*PinnedVersion  `json:"pinnedVersions"` // ws/env/type/logical
	ModelAllowlist map[string][]ModelAllowEntry `json:"modelAllowlist"` // wsID
	AIRevocations  map[string]string            `json:"aiRevocations"`  // ws/env/type/logical -> reason

	Quotas map[string]Quotas `json:"quotas"` // wsID; missing = DefaultQuotas

	// HMACKey is the CURRENT pseudonymization key (last ring entry), kept as
	// a top-level field for store-file compatibility.
	HMACKey string `json:"hmacKey"`
	// HMACKeyRing is the ordered key chain (oldest first). HashUnit applies
	// every layer, so rotating the key extends the ring and a migration
	// re-hashes stored pseudonyms with the new layer (RotateHMACKey,
	// §10.2: 单元 ID 做租户范围内 HMAC，密钥轮换有迁移方案). A single-entry
	// ring hashes identically to the historical single-key scheme.
	HMACKeyRing []string `json:"hmacKeyRing,omitempty"`
}

// Store is the concurrency-safe platform store. Reads are served from the
// in-memory working set; every mutation is durably committed through the
// Persistence backend as one transactional unit (file JSON for dev, or
// PostgreSQL for production — docs/技术方案.md §3, §10.2).
type Store struct {
	mu        sync.RWMutex
	persist   Persistence
	dedup     eventDedup // optional backend dedup ledger (PostgreSQL)
	changes   *ChangeSet
	data      *Data
	syncStats syncMetrics
}

const dataSchemaVersion = 1

// MaxRetainedEvents caps in-memory raw event retention (aggregates are
// unbounded). Oldest events are evicted first; the cap exists so the
// working set cannot exhaust memory. The PostgreSQL backend retains raw
// events for the full retention window regardless of this cap (§10.2).
const MaxRetainedEvents = 200_000

// NewStore opens (or creates) a store. When path != "", state is loaded from
// the JSON file if present and persisted atomically on every mutation.
func NewStore(path string) (*Store, error) {
	if path == "" {
		return newStore(nopPersistence{})
	}
	return newStore(&filePersistence{path: path})
}

// NewPGStore opens (or creates) a PostgreSQL-backed store. The DSN is a
// standard postgres connection string; the schema is migrated automatically
// and the full state is loaded into memory on open (production path, §10.2).
func NewPGStore(ctx context.Context, dsn string) (*Store, error) {
	p, err := newPGPersistence(ctx, dsn)
	if err != nil {
		return nil, err
	}
	s, err := newStore(p)
	if err != nil {
		_ = p.Close()
		return nil, err
	}
	return s, nil
}

func newStore(p Persistence) (*Store, error) {
	s := &Store{persist: p, changes: newChangeSet()}
	if dd, ok := p.(eventDedup); ok {
		s.dedup = dd
	}
	d, err := p.Load()
	if err != nil {
		return nil, err
	}
	if d == nil {
		d = newData()
	}
	s.data = d
	if err := s.normalizeKeyRing(); err != nil {
		return nil, err
	}
	// Persist bootstrap state (counters + key ring) — matches the historical
	// behavior of NewStore writing the store file on open.
	s.changes.Meta = true
	if err := s.commitLocked(); err != nil {
		if !errors.Is(err, ErrConflict) {
			return nil, err
		}
		// Another replica initialized or advanced the shared store while we
		// opened it. Reloaded by commitLocked; no bootstrap write is needed.
	}
	return s, nil
}

// normalizeKeyRing migrates single-key state to the ring representation and
// generates the deployment key when none exists yet.
func (s *Store) normalizeKeyRing() error {
	d := s.data
	if len(d.HMACKeyRing) == 0 {
		if d.HMACKey != "" {
			d.HMACKeyRing = []string{d.HMACKey}
		} else {
			key := make([]byte, 32)
			if _, err := rand.Read(key); err != nil {
				return fmt.Errorf("platform: generate HMAC key: %w", err)
			}
			d.HMACKeyRing = []string{hex.EncodeToString(key)}
		}
	}
	d.HMACKey = d.HMACKeyRing[len(d.HMACKeyRing)-1]
	return nil
}

// Close releases backend resources (connection pools, file handles).
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dedup = nil
	if s.persist == nil {
		return nil
	}
	err := s.persist.Close()
	s.persist = nopPersistence{}
	return err
}

func newData() *Data {
	return &Data{
		SchemaVersion:     dataSchemaVersion,
		NextCounter:       1,
		Workspaces:        map[string]*Workspace{},
		EnvBySlugKey:      map[string]string{},
		Environments:      map[string]*Environment{},
		Tokens:            map[string]*Token{},
		TokensByHash:      map[string]string{},
		FlagDrafts:        map[string]*FlagDraft{},
		FlagRevisions:     map[string][]*FlagRevision{},
		CurrentFlagRev:    map[string]int64{},
		NextFlagRev:       map[string]int64{},
		FlagTombstones:    map[string]int64{},
		SegmentDrafts:     map[string]*SegmentDraft{},
		SegmentRevisions:  map[string][]*SegmentRevision{},
		CurrentSegmentRev: map[string]int64{},
		NextSegmentRev:    map[string]int64{},
		ManifestRevisions: map[string]int64{},
		Experiments:       map[string]*Experiment{},
		EventIndex:        map[string]int{},
		UnitAggs:          map[string]*UnitAgg{},
		ExposedUnits:      map[string]map[string]bool{},
		SplitExposedUnits: map[string]map[string]bool{},
		EventCounts:       map[string]int64{},
		AIResources:       map[string]*AIResourceRef{},
		PinnedVersions:    map[string][]*PinnedVersion{},
		ModelAllowlist:    map[string][]ModelAllowEntry{},
		AIRevocations:     map[string]string{},
		Quotas:            map[string]Quotas{},
	}
}

// commitLocked persists the accumulated ChangeSet as one transactional unit.
// Caller holds mu. On failure the ChangeSet is RETAINED and replayed with the
// next commit, so backends only ever see idempotent operations.
func (s *Store) commitLocked() error {
	if s.persist == nil || s.changes.empty() {
		return nil
	}
	if err := s.persist.Commit(s.data, s.changes); err != nil {
		if errors.Is(err, ErrConflict) {
			// The attempted mutation did not commit. Discard its working-set
			// changes before another request can see or replay them.
			if d, loadErr := s.persist.Load(); loadErr == nil && d != nil {
				s.data = d
				s.changes = newChangeSet()
			} else if loadErr != nil {
				return fmt.Errorf("platform: reload after conflict: %w (original: %v)", loadErr, err)
			}
		}
		return fmt.Errorf("platform: persist: %w", err)
	}
	s.changes = newChangeSet()
	return nil
}

// nextID returns a server-generated surrogate ID with the given prefix.
// Caller holds the write lock.
func (s *Store) nextID(prefix string) string {
	id := fmt.Sprintf("%s-%d", prefix, s.data.NextCounter)
	s.data.NextCounter++
	s.changes.Meta = true
	return id
}

func (s *Store) now() time.Time { return time.Now().UTC() }

// ---------------------------------------------------------------------------
// Workspaces & environments
// ---------------------------------------------------------------------------

// ValidSlug reports whether s is an acceptable user-facing identifier.
func ValidSlug(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
		case c >= 'A' && c <= 'Z':
			return false // keep slugs lowercase-canonical
		default:
			return false
		}
		if i == 0 && (c < 'a' || c > 'z') {
			return false
		}
	}
	return true
}

// CreateWorkspace registers a new tenant.
func (s *Store) CreateWorkspace(slug, name string) (*Workspace, error) {
	if !ValidSlug(slug) {
		return nil, fmt.Errorf("%w: workspace slug %q must be [a-z0-9-], start with a letter, ≤64 chars", ErrValidation, slug)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range s.data.Workspaces {
		if w.Slug == slug {
			return nil, fmt.Errorf("%w: workspace %q", ErrExists, slug)
		}
	}
	w := &Workspace{ID: s.nextID("ws"), Slug: slug, Name: name, CreatedAt: s.now()}
	if w.Name == "" {
		w.Name = slug
	}
	s.data.Workspaces[w.ID] = w
	s.changes.markDirty(collWorkspaces, w.ID)
	return w, s.commitLocked()
}

// GetWorkspace resolves by ID or slug.
func (s *Store) GetWorkspace(idOrSlug string) (*Workspace, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if w, ok := s.data.Workspaces[idOrSlug]; ok {
		return w, nil
	}
	for _, w := range s.data.Workspaces {
		if w.Slug == idOrSlug {
			return w, nil
		}
	}
	return nil, fmt.Errorf("%w: workspace %q", ErrNotFound, idOrSlug)
}

// ListWorkspaces returns all workspaces ordered by slug.
func (s *Store) ListWorkspaces() []*Workspace {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Workspace, 0, len(s.data.Workspaces))
	for _, w := range s.data.Workspaces {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out
}

// CreateEnvironment registers an environment and its gonacos namespace
// mapping. The namespace ID is generated from server surrogate IDs only.
func (s *Store) CreateEnvironment(workspaceID, slug, name string) (*Environment, error) {
	if !ValidSlug(slug) {
		return nil, fmt.Errorf("%w: environment slug %q must be [a-z0-9-], start with a letter, ≤64 chars", ErrValidation, slug)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ws, ok := s.data.Workspaces[workspaceID]
	if !ok {
		return nil, fmt.Errorf("%w: workspace %q", ErrNotFound, workspaceID)
	}
	slugKey := ws.ID + "/" + slug
	if _, dup := s.data.EnvBySlugKey[slugKey]; dup {
		return nil, fmt.Errorf("%w: environment %q in workspace %q", ErrExists, slug, ws.Slug)
	}
	env := &Environment{
		ID: s.nextID("env"), WorkspaceID: ws.ID, Slug: slug, Name: name,
		CreatedAt: s.now(),
	}
	if env.Name == "" {
		env.Name = slug
	}
	// Namespace ID: server-generated components only (§5.1).
	env.NamespaceID = "golab-" + ws.ID + "-" + env.ID
	s.data.Environments[env.ID] = env
	s.data.EnvBySlugKey[slugKey] = env.ID
	s.changes.markDirty(collEnvironments, env.ID)
	return env, s.commitLocked()
}

// GetEnvironment resolves by ID, or by (workspaceID, slug).
func (s *Store) GetEnvironment(workspaceID, idOrSlug string) (*Environment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if e, ok := s.data.Environments[idOrSlug]; ok && (workspaceID == "" || e.WorkspaceID == workspaceID) {
		return e, nil
	}
	if eid, ok := s.data.EnvBySlugKey[workspaceID+"/"+idOrSlug]; ok {
		return s.data.Environments[eid], nil
	}
	return nil, fmt.Errorf("%w: environment %q", ErrNotFound, idOrSlug)
}

// ListEnvironments returns environments of one workspace ordered by slug.
func (s *Store) ListEnvironments(workspaceID string) []*Environment {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []*Environment{}
	for _, e := range s.data.Environments {
		if e.WorkspaceID == workspaceID {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out
}

// ---------------------------------------------------------------------------
// Tokens (§9: separate audiences; workspace/environment derived from the
// credential, never from request parameters)
// ---------------------------------------------------------------------------

// CreatedToken is returned once at creation: the plaintext secret is never
// retrievable afterwards.
type CreatedToken struct {
	Token  *Token
	Secret string
}

// CreateToken mints a new credential. The secret format is
// glab_<kind>_<43 url-safe chars>; only its SHA-256 is stored.
func (s *Store) CreateToken(kind TokenKind, name string, role Role, workspaceID, environmentID, createdBy string) (*CreatedToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch kind {
	case TokenAdmin:
		switch role {
		case RoleOwner, RolePublisher, RoleAnalyst, RoleViewer:
		default:
			return nil, fmt.Errorf("%w: admin token role %q", ErrValidation, role)
		}
		if workspaceID != "" {
			if _, ok := s.data.Workspaces[workspaceID]; !ok {
				return nil, fmt.Errorf("%w: workspace %q", ErrNotFound, workspaceID)
			}
		} else if role != RoleOwner {
			return nil, fmt.Errorf("%w: only owner tokens may be global", ErrValidation)
		}
	case TokenSDK, TokenCollect:
		role = ""
		if workspaceID == "" || environmentID == "" {
			return nil, fmt.Errorf("%w: %s tokens must be scoped to workspace+environment", ErrValidation, kind)
		}
		env, ok := s.data.Environments[environmentID]
		if !ok || env.WorkspaceID != workspaceID {
			return nil, fmt.Errorf("%w: environment %q not in workspace", ErrNotFound, environmentID)
		}
	default:
		return nil, fmt.Errorf("%w: token kind %q", ErrValidation, kind)
	}
	if name == "" {
		return nil, fmt.Errorf("%w: token name required", ErrValidation)
	}

	secretRaw := make([]byte, 32)
	if _, err := rand.Read(secretRaw); err != nil {
		return nil, err
	}
	secret := fmt.Sprintf("glab_%s_%s", kind, base64.RawURLEncoding.EncodeToString(secretRaw))
	hash := hashSecret(secret)

	tok := &Token{
		ID: s.nextID("tok"), Name: name, Kind: kind, Role: role,
		WorkspaceID: workspaceID, EnvironmentID: environmentID,
		SecretHash: hash, Prefix: secret[:14],
		CreatedAt: s.now(), CreatedBy: createdBy,
	}
	s.data.Tokens[tok.ID] = tok
	s.data.TokensByHash[hash] = tok.ID
	s.changes.markDirty(collTokens, tok.ID)
	if err := s.commitLocked(); err != nil {
		return nil, err
	}
	return &CreatedToken{Token: tok, Secret: secret}, nil
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// ResolveToken authenticates a presented secret and returns the token
// metadata. Revoked or unknown tokens yield ErrAccessDenied.
//
// PostgreSQL looks up the token in the database on every request so revocation
// on another replica takes effect immediately. File/memory stores update
// LastUsedAt in memory with a 60s throttle; that touch rides along with the
// next mutation that saves.
func (s *Store) ResolveToken(secret string) (*Token, error) {
	hash := hashSecret(secret)
	if lookup, ok := s.persist.(interface{ LookupToken(string) (*Token, error) }); ok {
		return lookup.LookupToken(hash)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.data.TokensByHash[hash]
	if !ok {
		return nil, fmt.Errorf("%w: unknown token", ErrAccessDenied)
	}
	tok := s.data.Tokens[id]
	if tok.Revoked {
		return nil, fmt.Errorf("%w: token revoked", ErrAccessDenied)
	}
	now := s.now()
	if now.Sub(tok.LastUsedAt) >= time.Minute {
		tok.LastUsedAt = now
		// Marked dirty but NOT committed here: the touch rides along with
		// the next mutation that saves (see doc comment above).
		s.changes.markDirty(collTokens, tok.ID)
	}
	return tok, nil
}

// RevokeToken disables a credential.
func (s *Store) RevokeToken(tokenID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tok, ok := s.data.Tokens[tokenID]
	if !ok {
		return fmt.Errorf("%w: token %q", ErrNotFound, tokenID)
	}
	tok.Revoked = true
	s.changes.markDirty(collTokens, tok.ID)
	return s.commitLocked()
}

// ListTokens returns metadata (never hashes/secrets) for one workspace, or
// all when workspaceID is empty.
func (s *Store) ListTokens(workspaceID string) []*Token {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []*Token{}
	for _, t := range s.data.Tokens {
		if workspaceID == "" || t.WorkspaceID == workspaceID || (t.Kind == TokenAdmin && t.WorkspaceID == "") {
			cp := *t
			cp.SecretHash = "" // never leave the store
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ---------------------------------------------------------------------------
// Audit
// ---------------------------------------------------------------------------

// Audit appends an immutable audit record.
func (s *Store) Audit(actor string, role Role, action, workspaceID, environmentID, object, detail string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for attempt := range 20 {
		entry := &AuditEntry{
			ID: s.data.NextAudit, Time: s.now(), Actor: actor, ActorRole: role,
			Action: action, WorkspaceID: workspaceID, EnvironmentID: environmentID,
			Object: object, Detail: detail,
		}
		s.data.NextAudit++
		s.data.Audit = append(s.data.Audit, entry)
		s.changes.Audit = append(s.changes.Audit, entry)
		s.changes.Meta = true
		err := s.commitLocked()
		if !errors.Is(err, ErrConflict) || attempt == 19 {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * time.Millisecond)
	}
	return nil
}

// AuditList returns the most recent entries first, limited to n (0 = all).
func (s *Store) AuditList(workspaceID string, n int) []*AuditEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*AuditEntry, 0, len(s.data.Audit))
	for i := len(s.data.Audit) - 1; i >= 0; i-- {
		e := s.data.Audit[i]
		if workspaceID == "" || e.WorkspaceID == workspaceID {
			out = append(out, e)
			if n > 0 && len(out) >= n {
				break
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Quotas & pseudonymization
// ---------------------------------------------------------------------------

// SetQuotas configures per-workspace limits.
func (s *Store) SetQuotas(workspaceID string, q Quotas) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data.Workspaces[workspaceID]; !ok {
		return fmt.Errorf("%w: workspace %q", ErrNotFound, workspaceID)
	}
	s.data.Quotas[workspaceID] = q
	s.changes.markDirty(collQuotas, workspaceID)
	return s.commitLocked()
}

// GetQuotas returns configured or default quotas.
func (s *Store) GetQuotas(workspaceID string) Quotas {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if q, ok := s.data.Quotas[workspaceID]; ok {
		return q
	}
	return DefaultQuotas()
}

// HashUnit pseudonymizes a randomization unit within this deployment
// (tenant-scoped HMAC, §10.2). The hash chains through the full key ring, so
// a single-key ring is byte-identical to the historical scheme and rotation
// preserves unit identity: RotateHMACKey re-hashes stored pseudonyms with
// exactly the layer new events now gain.
func (s *Store) HashUnit(workspaceID, unit string) string {
	s.mu.RLock()
	ring := s.data.HMACKeyRing
	s.mu.RUnlock()
	h := unit
	for _, key := range ring {
		h = hashUnitLayer(key, workspaceID, h)
	}
	return h
}

// hashUnitLayer applies one HMAC ring layer: hex(HMAC-SHA256(key, ws||0||v)).
// Exported to no one — but shared by RotateHMACKey and the PostgreSQL
// migration so memory and storage can never drift apart.
func hashUnitLayer(key, workspaceID, v string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(workspaceID))
	mac.Write([]byte{0})
	mac.Write([]byte(v))
	return hex.EncodeToString(mac.Sum(nil))
}

// RotateHMACKey appends a new pseudonymization key to the ring and migrates
// every stored unit hash (raw events, exposed-unit sets, unit aggregates)
// through the new layer — the 密钥轮换迁移路径 of §10.2. Rotation is a
// migration, not a hot swap: it runs synchronously, is atomic per backend
// commit, and preserves unit identity linkage (the chain property of
// HashUnit means new events for the same unit land on the SAME migrated
// hash). The new key must be 32 bytes of hex; old keys stay in the ring as
// chain layers (each layer alone is useless for reversing the final hash).
func (s *Store) RotateHMACKey(newKeyHex string) error {
	if _, err := hex.DecodeString(newKeyHex); err != nil || len(newKeyHex) != 64 {
		return fmt.Errorf("%w: HMAC key must be 64 hex chars (32 bytes)", ErrValidation)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ring := s.data.HMACKeyRing
	if len(ring) > 0 && ring[len(ring)-1] == newKeyHex {
		return nil // already current
	}
	// Re-hash every stored pseudonym in memory with the new layer.
	for _, ev := range s.data.Events {
		ev.UnitHash = hashUnitLayer(newKeyHex, ev.WorkspaceID, ev.UnitHash)
	}
	s.data.UnitAggs = rehashAggKeys(s.data.UnitAggs, newKeyHex)
	s.data.ExposedUnits = rehashUnitSets(s.data.ExposedUnits, newKeyHex)
	s.data.SplitExposedUnits = rehashUnitSets(s.data.SplitExposedUnits, newKeyHex)
	// Pending (not yet committed) aggregate upserts carry unit hashes as
	// plain copies — rotate them too so a commit-failure window can never
	// persist pre-rotation hashes after the migration ran.
	for i := range s.changes.Aggs {
		a := &s.changes.Aggs[i]
		a.UnitHash = hashUnitLayer(newKeyHex, a.WorkspaceID, a.UnitHash)
	}
	s.data.HMACKeyRing = append(append([]string(nil), ring...), newKeyHex)
	s.data.HMACKey = newKeyHex
	s.changes.Meta = true
	s.changes.Rotation = true
	return s.commitLocked()
}

// rehashAggKeys rebuilds the UnitAggs map with rotated unit hashes. The agg
// key format is ws/env/exp/epoch/variant/unitHash/metric (the same layout
// MetricAggregate parses): the workspace is the first segment and the unit
// hash the sixth — surrogate IDs never contain '/', and metric keys keep any
// '/' inside the trailing segment.
func rehashAggKeys(aggs map[string]*UnitAgg, newKeyHex string) map[string]*UnitAgg {
	out := make(map[string]*UnitAgg, len(aggs))
	for key, agg := range aggs {
		parts := splitN(key, '/', 7)
		if len(parts) < 7 {
			out[key] = agg // malformed key; cannot exist by construction
			continue
		}
		parts[5] = hashUnitLayer(newKeyHex, parts[0], parts[5])
		joined := parts[0]
		for _, p := range parts[1:] {
			joined += "/" + p
		}
		out[joined] = agg
	}
	return out
}

// rehashUnitSets rebuilds exposed-unit set maps (scope → unitHash set).
func rehashUnitSets(sets map[string]map[string]bool, newKeyHex string) map[string]map[string]bool {
	out := make(map[string]map[string]bool, len(sets))
	for scope, set := range sets {
		ws := scope
		if j := indexOf(scope, '/'); j >= 0 {
			ws = scope[:j]
		}
		rehashed := make(map[string]bool, len(set))
		for h := range set {
			rehashed[hashUnitLayer(newKeyHex, ws, h)] = true
		}
		out[scope] = rehashed
	}
	return out
}

func indexOf(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func splitN(s string, sep byte, n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < len(s); {
		if len(out) == n-1 {
			out = append(out, s[i:])
			return out
		}
		j := indexOf(s[i:], sep)
		if j < 0 {
			out = append(out, s[i:])
			return out
		}
		out = append(out, s[i:i+j])
		i += j + 1
	}
	return out
}
