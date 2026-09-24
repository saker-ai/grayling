package platform

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

	// HMACKey pseudonymizes randomization units per deployment (§10.2:
	// 单元 ID 做租户范围内 HMAC). Rotation requires re-hashing stored
	// events — a migration, not a hot swap; kept out of API responses.
	HMACKey string `json:"hmacKey"`
}

// Store is the concurrency-safe platform store.
type Store struct {
	mu   sync.RWMutex
	path string // "" = memory-only
	data *Data
}

const dataSchemaVersion = 1

// MaxRetainedEvents caps in-memory raw event retention (aggregates are
// unbounded). Oldest events are evicted first; the cap exists so the dev
// store cannot exhaust memory — production retention lives in PostgreSQL
// (§10.2).
const MaxRetainedEvents = 200_000

// NewStore opens (or creates) a store. When path != "", state is loaded from
// the JSON file if present and persisted atomically on every mutation.
func NewStore(path string) (*Store, error) {
	s := &Store{path: path}
	if path != "" {
		if err := s.load(); err != nil {
			return nil, err
		}
	}
	if s.data == nil {
		s.data = newData()
	}
	if s.data.HMACKey == "" {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("platform: generate HMAC key: %w", err)
		}
		s.data.HMACKey = hex.EncodeToString(key)
	}
	if path != "" {
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
	}
	return s, nil
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

func (s *Store) load() error {
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("platform: read %s: %w", s.path, err)
	}
	var d Data
	if err := json.Unmarshal(raw, &d); err != nil {
		return fmt.Errorf("platform: parse %s: %w", s.path, err)
	}
	if d.SchemaVersion != dataSchemaVersion {
		return fmt.Errorf("platform: store file schema %d, want %d", d.SchemaVersion, dataSchemaVersion)
	}
	s.data = &d
	return nil
}

// saveLocked persists state atomically (tmp file + rename). Caller holds mu.
func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	raw, err := json.Marshal(s.data)
	if err != nil {
		return fmt.Errorf("platform: encode state: %w", err)
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".golab-store-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil { // contains HMAC key material
		return err
	}
	return os.Rename(tmpName, s.path)
}

// nextID returns a server-generated surrogate ID with the given prefix.
// Caller holds the write lock.
func (s *Store) nextID(prefix string) string {
	id := fmt.Sprintf("%s-%d", prefix, s.data.NextCounter)
	s.data.NextCounter++
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
	return w, s.saveLocked()
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
	return env, s.saveLocked()
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
	if err := s.saveLocked(); err != nil {
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
// LastUsedAt is updated in memory with a 60s throttle and WITHOUT forcing a
// disk save: SDK/collect endpoints authenticate on every request, and a
// full-store persist per request would dominate latency. The touch rides
// along with the next mutation that saves.
func (s *Store) ResolveToken(secret string) (*Token, error) {
	hash := hashSecret(secret)
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
	return s.saveLocked()
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
	entry := &AuditEntry{
		ID: s.data.NextAudit, Time: s.now(), Actor: actor, ActorRole: role,
		Action: action, WorkspaceID: workspaceID, EnvironmentID: environmentID,
		Object: object, Detail: detail,
	}
	s.data.NextAudit++
	s.data.Audit = append(s.data.Audit, entry)
	return s.saveLocked()
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
	return s.saveLocked()
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
// (tenant-scoped HMAC, §10.2). Deterministic per store lifetime/key.
func (s *Store) HashUnit(workspaceID, unit string) string {
	s.mu.RLock()
	key := s.data.HMACKey
	s.mu.RUnlock()
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(workspaceID))
	mac.Write([]byte{0})
	mac.Write([]byte(unit))
	return hex.EncodeToString(mac.Sum(nil))
}
