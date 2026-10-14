package platform

import (
	"fmt"
	"sort"
	"strings"
)

// AI resource types Grayling brokers. gonacos versions prompt/skill/agentspec
// natively; mcp/pipeline lack immutable-version semantics there, so Grayling
// only allows *referencing* them by their own recorded digest and refuses
// version pinning until an adapter provides immutable reads (§7: MCP、
// pipeline 等资源只有在对应适配器支持不可变版本读取和授权校验后才可发布).
const (
	AITypePrompt    = "prompt"
	AITypeSkill     = "skill"
	AITypeAgentSpec = "agentspec"
	AITypeMCP       = "mcp"
	AITypePipeline  = "pipeline"
)

// VersionedAITypes can be pinned to immutable gonacos versions.
var VersionedAITypes = map[string]bool{
	AITypePrompt: true, AITypeSkill: true, AITypeAgentSpec: true,
}

func aiKey(workspaceID, environmentID, typ, logicalID string) string {
	return strings.Join([]string{workspaceID, environmentID, typ, logicalID}, "\x00")
}

// RegisterAIResource creates the ownership index entry and allocates the
// server-generated physical gonacos ID. Physical IDs are globally unique
// across workspaces because gonacos AI IDs have no namespace (§5.1): the
// index, not the substrate, provides tenant isolation.
func (s *Store) RegisterAIResource(workspaceID, environmentID, typ, logicalID, owner string) (*AIResourceRef, error) {
	if !VersionedAITypes[typ] && typ != AITypeMCP && typ != AITypePipeline {
		return nil, fmt.Errorf("%w: unsupported AI resource type %q", ErrValidation, typ)
	}
	if !ValidSlug(strings.ToLower(logicalID)) {
		return nil, fmt.Errorf("%w: logical ID %q must be [a-z0-9-]", ErrValidation, logicalID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkEnvLocked(workspaceID, environmentID); err != nil {
		return nil, err
	}
	key := aiKey(workspaceID, environmentID, typ, logicalID)
	if _, exists := s.data.AIResources[key]; exists {
		return nil, fmt.Errorf("%w: %s/%q already registered in this environment", ErrExists, typ, logicalID)
	}
	ref := &AIResourceRef{
		WorkspaceID: workspaceID, EnvironmentID: environmentID,
		Type: typ, LogicalID: logicalID, Owner: owner,
		// Physical ID embeds the surrogate IDs — never user text beyond the
		// validated slug — and a counter for global uniqueness.
		PhysicalID: fmt.Sprintf("grayling-%s-%s-%s-%d", workspaceID, environmentID, logicalID, s.data.NextCounter),
		CreatedAt:  s.now(),
	}
	s.data.NextCounter++
	s.changes.Meta = true
	s.data.AIResources[key] = ref
	s.changes.markDirty(collAIResources, key)
	return ref, s.commitLocked()
}

// GetAIResource resolves one ownership index entry. Cross-workspace lookups
// are impossible by construction: the key includes workspace+environment.
func (s *Store) GetAIResource(workspaceID, environmentID, typ, logicalID string) (*AIResourceRef, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ref, ok := s.data.AIResources[aiKey(workspaceID, environmentID, typ, logicalID)]
	if !ok {
		return nil, fmt.Errorf("%w: AI resource %s/%q", ErrNotFound, typ, logicalID)
	}
	return ref, nil
}

// GetAIResourceByPhysical resolves via physical ID and returns the owning
// scope — used by the authorization gate to check that the CALLER's scope
// matches the owner scope before any gonacos read (§5.1: 所有 AI 读写走
// Grayling 授权入口).
func (s *Store) GetAIResourceByPhysical(physicalID string) (*AIResourceRef, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, ref := range s.data.AIResources {
		if ref.PhysicalID == physicalID {
			return ref, nil
		}
	}
	return nil, fmt.Errorf("%w: physical ID %q", ErrNotFound, physicalID)
}

// ListAIResources returns the index of one environment.
func (s *Store) ListAIResources(workspaceID, environmentID string) []*AIResourceRef {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []*AIResourceRef{}
	for _, ref := range s.data.AIResources {
		if ref.WorkspaceID == workspaceID && (environmentID == "" || ref.EnvironmentID == environmentID) {
			out = append(out, ref)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		return out[i].LogicalID < out[j].LogicalID
	})
	return out
}

// DeleteAIResource removes the index entry (the gonacos-side object is
// handled by the adapter; revoking the index entry immediately makes the
// resource unresolvable through Grayling authorization).
func (s *Store) DeleteAIResource(workspaceID, environmentID, typ, logicalID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := aiKey(workspaceID, environmentID, typ, logicalID)
	if _, ok := s.data.AIResources[key]; !ok {
		return fmt.Errorf("%w: AI resource %s/%q", ErrNotFound, typ, logicalID)
	}
	delete(s.data.AIResources, key)
	s.changes.markDeleted(collAIResources, key)
	return s.commitLocked()
}

// PinAIVersion records an immutable version binding (§7: 默认发布时把 label
// 解析为明确的 version 与 digest，快照中固定该版本). The digest is verified
// by the caller against gonacos at pin time AND at resolve time — content
// drift fails closed.
func (s *Store) PinAIVersion(pin *PinnedVersion) error {
	if pin == nil || pin.Version == "" || pin.Digest == "" {
		return fmt.Errorf("%w: pin needs version and digest", ErrValidation)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := aiKey(pin.WorkspaceID, pin.EnvironmentID, pin.Type, pin.LogicalID)
	ref, ok := s.data.AIResources[key]
	if !ok {
		return fmt.Errorf("%w: AI resource %s/%q not registered", ErrNotFound, pin.Type, pin.LogicalID)
	}
	if ref.PhysicalID != pin.PhysicalID {
		return fmt.Errorf("%w: physical ID mismatch for %s/%q", ErrValidation, pin.Type, pin.LogicalID)
	}
	for _, existing := range s.data.PinnedVersions[key] {
		if existing.Version == pin.Version {
			if existing.Digest != pin.Digest {
				return fmt.Errorf("%w: version %q of %s/%q already pinned with different digest",
					ErrConflict, pin.Version, pin.Type, pin.LogicalID)
			}
			return nil // idempotent re-pin of identical content
		}
	}
	if pin.PinnedAt.IsZero() {
		pin.PinnedAt = s.now()
	}
	s.data.PinnedVersions[key] = append(s.data.PinnedVersions[key], pin)
	s.changes.markDirty(collPinnedVersions, key)
	return s.commitLocked()
}

// GetPinnedVersion returns the recorded digest for one pinned version.
func (s *Store) GetPinnedVersion(workspaceID, environmentID, typ, logicalID, version string) (*PinnedVersion, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.data.PinnedVersions[aiKey(workspaceID, environmentID, typ, logicalID)] {
		if p.Version == version {
			return p, nil
		}
	}
	return nil, fmt.Errorf("%w: pinned version %q of %s/%q", ErrNotFound, version, typ, logicalID)
}

// ListPinnedVersions returns all pins of one logical resource.
func (s *Store) ListPinnedVersions(workspaceID, environmentID, typ, logicalID string) []*PinnedVersion {
	s.mu.RLock()
	defer s.mu.RUnlock()
	src := s.data.PinnedVersions[aiKey(workspaceID, environmentID, typ, logicalID)]
	out := make([]*PinnedVersion, len(src))
	copy(out, src)
	return out
}

// SetModelAllowlist replaces the provider/model allowlist of one workspace
// (§7: modelRef 是 Grayling 适配器维护的白名单).
func (s *Store) SetModelAllowlist(workspaceID string, entries []ModelAllowEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data.Workspaces[workspaceID]; !ok {
		return fmt.Errorf("%w: workspace %q", ErrNotFound, workspaceID)
	}
	for i := range entries {
		entries[i].WorkspaceID = workspaceID
		if entries[i].Provider == "" || entries[i].Model == "" {
			return fmt.Errorf("%w: provider and model required", ErrValidation)
		}
	}
	s.data.ModelAllowlist[workspaceID] = entries
	s.changes.markDirty(collModelAllowlist, workspaceID)
	return s.commitLocked()
}

// ModelAllowed checks one (provider, model) pair against the allowlist.
func (s *Store) ModelAllowed(workspaceID, provider, modelName string) (ModelAllowEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.data.ModelAllowlist[workspaceID] {
		if e.Provider == provider && e.Model == modelName && e.Enabled {
			return e, true
		}
	}
	return ModelAllowEntry{}, false
}

// ListModelAllowlist returns the workspace allowlist.
func (s *Store) ListModelAllowlist(workspaceID string) []ModelAllowEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ModelAllowEntry, len(s.data.ModelAllowlist[workspaceID]))
	copy(out, s.data.ModelAllowlist[workspaceID])
	return out
}

// ---------------------------------------------------------------------------
// Emergency revocation (§7: 紧急安全撤销需另设可即时阻断的执行前校验，不
// 依赖普通快照). Revocations live in the control-plane store and are checked
// LIVE at every resolve/execute-gate call — never distributed via snapshots.
// ---------------------------------------------------------------------------

// RevokeAIResource blocks all resolution of one logical resource
// immediately.
func (s *Store) RevokeAIResource(workspaceID, environmentID, typ, logicalID, reason, actor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.AIRevocations == nil {
		s.data.AIRevocations = map[string]string{}
	}
	key := aiKey(workspaceID, environmentID, typ, logicalID)
	s.data.AIRevocations[key] = fmt.Sprintf("%s (by %s)", reason, actor)
	s.changes.markDirty(collAIRevocations, key)
	return s.commitLocked()
}

// ReinstateAIResource lifts a revocation.
func (s *Store) ReinstateAIResource(workspaceID, environmentID, typ, logicalID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := aiKey(workspaceID, environmentID, typ, logicalID)
	if _, ok := s.data.AIRevocations[key]; !ok {
		return fmt.Errorf("%w: %s/%q not revoked", ErrNotFound, typ, logicalID)
	}
	delete(s.data.AIRevocations, key)
	s.changes.markDeleted(collAIRevocations, key)
	return s.commitLocked()
}

// IsAIRevoked reports the live revocation state.
func (s *Store) IsAIRevoked(workspaceID, environmentID, typ, logicalID string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	reason, ok := s.data.AIRevocations[aiKey(workspaceID, environmentID, typ, logicalID)]
	return reason, ok
}

// ListAIRevocations returns all active revocations of one environment.
func (s *Store) ListAIRevocations(workspaceID, environmentID string) map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]string{}
	prefix := workspaceID + "\x00" + environmentID + "\x00"
	for k, v := range s.data.AIRevocations {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			out[k[len(prefix):]] = v
		}
	}
	return out
}
