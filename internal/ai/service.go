// Package ai brokers all access to AI resources (prompts, skills, agent
// specs) stored in embedded gonacos or external Nacos config documents.
//
// gonacos AI resources are indexed by GLOBAL IDs without any namespace
// field, so tenant isolation cannot come from the substrate. GoLab provides
// it instead:
//
//   - every logical resource is registered in the platform ownership index
//     under (workspace, environment, type, logical_id) and mapped to a
//     server-generated physical gonacos ID that embeds no guessable
//     tenant data beyond the validated slug;
//   - ALL reads and writes go through this service — the raw gonacos
//     HTTP/gRPC surface must never be exposed to tenants (§5.1);
//   - published versions are pinned with their gonacos MD5 digest and every
//     resolve re-verifies the digest, so content drift fails closed (§7:
//     已发布版本内容不可漂移);
//   - emergency revocation is checked LIVE on every resolve and through the
//     explicit pre-execution gate — it never depends on snapshot sync (§7);
//   - mcp/pipeline resources have no immutable-version semantics in
//     gonacos, so hosting them is refused until an adapter provides it
//     (§7: 不以字符串 name@v1 代替验证).
package ai

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/godeps/golab/internal/configstore"
	"github.com/godeps/golab/internal/platform"
	gonacosai "github.com/godeps/gonacos/pkg/ai"
)

// Errors surfaced by the resolver.
var (
	ErrUnauthorized     = errors.New("ai: resource not resolvable in this scope")
	ErrRevoked          = errors.New("ai: resource revoked")
	ErrDigestDrift      = errors.New("ai: published content digest changed (refusing to serve)")
	ErrVersionNotPinned = errors.New("ai: version was never pinned by GoLab")
	ErrTypeUnsupported  = errors.New("ai: resource type not supported for managed hosting")
	ErrModelNotAllowed  = errors.New("ai: model not in workspace allowlist")
)

// Service gates all AI resource operations.
type Service struct {
	Store    *platform.Store
	Gonacos  *gonacosai.Service
	External configstore.Store

	mu          sync.RWMutex
	localRevoke map[string]bool // fast-path cache of live revocations
}

// NewService assembles the AI gate. gonacosAI may come from
// server.Services().AI.
func NewService(store *platform.Store, gonacosAI *gonacosai.Service) *Service {
	return &Service{Store: store, Gonacos: gonacosAI, localRevoke: map[string]bool{}}
}

// NewExternalService stores immutable AI versions as Nacos config documents.
func NewExternalService(store *platform.Store, cfg configstore.Store) *Service {
	return &Service{Store: store, External: cfg, localRevoke: map[string]bool{}}
}

// Content is one resolved, digest-verified resource version.
type Content struct {
	Type       string    `json:"type"`
	LogicalID  string    `json:"logicalId"`
	PhysicalID string    `json:"physicalId"`
	Version    string    `json:"version"`
	Digest     string    `json:"digest"`
	Content    string    `json:"content"`
	ResolvedAt time.Time `json:"resolvedAt"`
}

// RegisterResource creates the ownership index entry. The physical gonacos
// ID is server-generated; callers never choose it.
func (s *Service) RegisterResource(workspaceID, environmentID, typ, logicalID, name, actor string) (*platform.AIResourceRef, error) {
	switch typ {
	case platform.AITypePrompt, platform.AITypeSkill, platform.AITypeAgentSpec:
	case platform.AITypeMCP, platform.AITypePipeline:
		return nil, fmt.Errorf("%w: %s lacks immutable-version semantics; managed hosting is disabled until an adapter provides it (§7)",
			ErrTypeUnsupported, typ)
	default:
		return nil, fmt.Errorf("%w: unknown type %q", ErrTypeUnsupported, typ)
	}
	ref, err := s.Store.RegisterAIResource(workspaceID, environmentID, typ, logicalID, actor)
	if err != nil {
		return nil, err
	}
	if s.External != nil {
		return ref, nil
	}
	// Create the gonacos-side placeholder draft so the physical ID exists.
	// Content is published through PublishVersion afterwards.
	switch typ {
	case platform.AITypePrompt:
		if _, err := s.Gonacos.CreatePromptDraft(ref.PhysicalID, name, "placeholder", actor,
			[]string{"golab-managed"}, nil, "GoLab-managed resource "+typ+"/"+logicalID, nil); err != nil {
			return nil, fmt.Errorf("ai: gonacos create prompt: %w", err)
		}
	case platform.AITypeSkill:
		if _, err := s.Gonacos.CreateSkillDraft(ref.PhysicalID, name, "placeholder", actor,
			[]string{"golab-managed"}, nil, "GoLab-managed resource "+typ+"/"+logicalID, nil); err != nil {
			return nil, fmt.Errorf("ai: gonacos create skill: %w", err)
		}
	case platform.AITypeAgentSpec:
		if _, err := s.Gonacos.CreateAgentSpecDraft(ref.PhysicalID, name, "placeholder", actor,
			[]string{"golab-managed"}, nil, "GoLab-managed resource "+typ+"/"+logicalID, nil); err != nil {
			return nil, fmt.Errorf("ai: gonacos create agentspec: %w", err)
		}
	}
	return ref, nil
}

// PublishVersion writes new content, publishes it as the next immutable
// gonacos version and pins (version, digest) in the GoLab index.
//
// gonacos lifecycle quirks handled here:
//   - Upload* helpers silently publish an already-open draft when
//     ErrDraftExists — dangerous with the registration placeholder, so we
//     drive the explicit Redraft/UpdateDraft + Publish(force) path instead;
//   - RedraftX requires PUBLISHED state; UpdateXDraft requires an open
//     draft — exactly one applies at any time, the other's error is
//     tolerated.
func (s *Service) PublishVersion(workspaceID, environmentID, typ, logicalID, content, actor string) (*platform.PinnedVersion, error) {
	ref, err := s.Store.GetAIResource(workspaceID, environmentID, typ, logicalID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	if strings.TrimSpace(content) == "" {
		return nil, fmt.Errorf("ai: refusing to publish empty content")
	}
	if s.External != nil {
		return s.publishExternal(workspaceID, environmentID, typ, logicalID, content, actor, ref)
	}
	var res *gonacosai.Resource
	switch typ {
	case platform.AITypePrompt:
		res, err = s.redraftUpdatePublish(
			func() (*gonacosai.Resource, error) { return s.Gonacos.RedraftPrompt(ref.PhysicalID, content, actor) },
			func() (*gonacosai.Resource, error) {
				return s.Gonacos.UpdatePromptDraft(ref.PhysicalID, content, actor, nil, nil, "", nil)
			},
			func() (*gonacosai.Resource, error) { return s.Gonacos.PublishPrompt(ref.PhysicalID, true) },
		)
	case platform.AITypeSkill:
		res, err = s.redraftUpdatePublish(
			func() (*gonacosai.Resource, error) { return s.Gonacos.RedraftSkill(ref.PhysicalID, content, actor) },
			func() (*gonacosai.Resource, error) {
				return s.Gonacos.UpdateSkillDraft(ref.PhysicalID, content, actor, nil, nil, "", nil)
			},
			func() (*gonacosai.Resource, error) { return s.Gonacos.PublishSkill(ref.PhysicalID, true) },
		)
	case platform.AITypeAgentSpec:
		res, err = s.redraftUpdatePublish(
			func() (*gonacosai.Resource, error) { return s.Gonacos.RedraftAgentSpec(ref.PhysicalID, content, actor) },
			func() (*gonacosai.Resource, error) {
				return s.Gonacos.UpdateAgentSpecDraft(ref.PhysicalID, content, actor, nil, nil, "", nil)
			},
			func() (*gonacosai.Resource, error) { return s.Gonacos.PublishAgentSpec(ref.PhysicalID, true) },
		)
	default:
		return nil, fmt.Errorf("%w: %s", ErrTypeUnsupported, typ)
	}
	if err != nil {
		return nil, err
	}
	version := res.CurrentVersion
	md5 := versionMD5(res, version)
	if version == "" || md5 == "" {
		return nil, fmt.Errorf("ai: gonacos did not return a published version/digest")
	}

	// Read-back verification before pinning: the version we just published
	// must download with exactly the digest gonacos reported (§3: 不报告
	// "已生效"直到读回验证).
	got, err := s.downloadVersion(workspaceID, environmentID, typ, ref.PhysicalID, version)
	if err != nil {
		return nil, fmt.Errorf("ai: read-back of %s@%s failed: %w", ref.PhysicalID, version, err)
	}
	if md5Hex(got) != md5 || got != content {
		return nil, fmt.Errorf("%w: read-back content differs from published digest", ErrDigestDrift)
	}

	pin := &platform.PinnedVersion{
		WorkspaceID: workspaceID, EnvironmentID: environmentID,
		Type: typ, LogicalID: logicalID, PhysicalID: ref.PhysicalID,
		Version: version, Digest: md5, PinnedBy: actor,
	}
	if err := s.Store.PinAIVersion(pin); err != nil {
		return nil, err
	}
	return pin, nil
}

func externalDataID(typ, physicalID, version string) string {
	return "golab.ai." + typ + "." + physicalID + "." + version
}

func (s *Service) publishExternal(workspaceID, environmentID, typ, logicalID, content, actor string, ref *platform.AIResourceRef) (*platform.PinnedVersion, error) {
	env, err := s.Store.GetEnvironment(workspaceID, environmentID)
	if err != nil {
		return nil, err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, fmt.Errorf("ai: create version ID: %w", err)
	}
	version := hex.EncodeToString(random[:])
	dataID := externalDataID(typ, ref.PhysicalID, version)
	if _, err := s.External.PublishDoc(env.NamespaceID, dataID, []byte(content)); err != nil {
		return nil, fmt.Errorf("ai: publish external version: %w", err)
	}
	pin := &platform.PinnedVersion{
		WorkspaceID: workspaceID, EnvironmentID: environmentID,
		Type: typ, LogicalID: logicalID, PhysicalID: ref.PhysicalID,
		Version: version, Digest: md5Hex(content), PinnedBy: actor,
	}
	if err := s.Store.PinAIVersion(pin); err != nil {
		return nil, err
	}
	return pin, nil
}

// redraftUpdatePublish drives the gonacos draft lifecycle across both
// possible states (open draft vs published) and force-publishes.
func (s *Service) redraftUpdatePublish(redraft, updateDraft, publish func() (*gonacosai.Resource, error)) (*gonacosai.Resource, error) {
	if _, err := redraft(); err != nil {
		// Not in a redraftable state — an open draft must exist (the
		// registration placeholder or a failed earlier attempt); update it.
		if _, uerr := updateDraft(); uerr != nil {
			return nil, fmt.Errorf("ai: gonacos draft update failed (redraft: %v; update: %v)", err, uerr)
		}
	}
	res, err := publish()
	if err != nil {
		return nil, fmt.Errorf("ai: gonacos publish: %w", err)
	}
	return res, nil
}

// md5Hex mirrors gonacos's internal digest (hex(md5(content))) — it is the
// digest recorded on every gonacos Version, so GoLab must compute it
// identically for drift checks.
func md5Hex(content string) string {
	sum := md5.Sum([]byte(content))
	return hex.EncodeToString(sum[:])
}

// versionMD5 extracts the digest gonacos recorded for one version.
func versionMD5(res *gonacosai.Resource, version string) string {
	if res == nil {
		return ""
	}
	for _, v := range res.Versions {
		if v.Version == version {
			return v.MD5
		}
	}
	return ""
}

// downloadVersion fetches version content through the gonacos API.
func (s *Service) downloadVersion(workspaceID, environmentID, typ, physicalID, version string) (string, error) {
	if s.External != nil {
		env, err := s.Store.GetEnvironment(workspaceID, environmentID)
		if err != nil {
			return "", err
		}
		content, _, err := s.External.GetDoc(env.NamespaceID, externalDataID(typ, physicalID, version))
		return string(content), err
	}
	switch typ {
	case platform.AITypePrompt:
		return s.Gonacos.DownloadPromptVersion(physicalID, version)
	case platform.AITypeSkill:
		return s.Gonacos.DownloadSkillVersion(physicalID, version)
	case platform.AITypeAgentSpec:
		// gonacos has no DownloadAgentSpecVersion; the in-process
		// GetAgentSpecVersion returns content in memory.
		v, err := s.Gonacos.GetAgentSpecVersion(physicalID, version)
		if err != nil {
			return "", err
		}
		return v.Content, nil
	}
	return "", fmt.Errorf("%w: %s", ErrTypeUnsupported, typ)
}

// Resolve returns one pinned, digest-verified version for the CALLER's
// scope. Unauthorized scopes (including any cross-workspace attempt) get
// ErrUnauthorized without leaking existence. Revocation is checked live.
func (s *Service) Resolve(workspaceID, environmentID, typ, logicalID, version string) (*Content, error) {
	if reason, revoked := s.revoked(workspaceID, environmentID, typ, logicalID); revoked {
		return nil, fmt.Errorf("%w: %s/%s: %s", ErrRevoked, typ, logicalID, reason)
	}
	ref, err := s.Store.GetAIResource(workspaceID, environmentID, typ, logicalID)
	if err != nil {
		return nil, ErrUnauthorized // never reveal whether it exists elsewhere
	}
	if version == "" {
		// Default resolution = newest pinned version. Labels are NOT
		// trusted as version pointers (§7: BindPromptLabel 不能证明唯一
		// 指向); only explicit pins resolve.
		pins := s.Store.ListPinnedVersions(workspaceID, environmentID, typ, logicalID)
		if len(pins) == 0 {
			return nil, fmt.Errorf("%w: no pinned versions for %s/%q", ErrVersionNotPinned, typ, logicalID)
		}
		version = pins[len(pins)-1].Version
	}
	pin, err := s.Store.GetPinnedVersion(workspaceID, environmentID, typ, logicalID, version)
	if err != nil {
		return nil, fmt.Errorf("%w: %s/%q@%s", ErrVersionNotPinned, typ, logicalID, version)
	}
	if pin.PhysicalID != ref.PhysicalID {
		return nil, ErrUnauthorized
	}
	content, err := s.downloadVersion(workspaceID, environmentID, typ, ref.PhysicalID, version)
	if err != nil {
		return nil, fmt.Errorf("ai: version %s@%s unreadable: %w", ref.PhysicalID, version, err)
	}
	// Digest re-verification on EVERY resolve: published content must never
	// drift, and if the substrate changed underneath us we fail closed.
	if actual := md5Hex(content); actual != pin.Digest {
		return nil, fmt.Errorf("%w: %s/%q@%s pinned digest %s, substrate now %s",
			ErrDigestDrift, typ, logicalID, version, pin.Digest, actual)
	}
	return &Content{
		Type: typ, LogicalID: logicalID, PhysicalID: ref.PhysicalID,
		Version: version, Digest: pin.Digest, Content: content,
		ResolvedAt: time.Now().UTC(),
	}, nil
}

// ResolveModelRef validates one modelRef against the workspace allowlist
// (§7: modelRef 是 GoLab 适配器维护的 provider/model/参数白名单).
func (s *Service) ResolveModelRef(workspaceID, provider, modelName string) (platform.ModelAllowEntry, error) {
	entry, ok := s.Store.ModelAllowed(workspaceID, provider, modelName)
	if !ok {
		return entry, fmt.Errorf("%w: %s/%s", ErrModelNotAllowed, provider, modelName)
	}
	return entry, nil
}

// Revoke blocks a resource immediately (live path, not snapshot-based).
func (s *Service) Revoke(workspaceID, environmentID, typ, logicalID, reason, actor string) error {
	if err := s.Store.RevokeAIResource(workspaceID, environmentID, typ, logicalID, reason, actor); err != nil {
		return err
	}
	s.mu.Lock()
	s.localRevoke[revokeKey(workspaceID, environmentID, typ, logicalID)] = true
	s.mu.Unlock()
	return nil
}

// Reinstate lifts a revocation.
func (s *Service) Reinstate(workspaceID, environmentID, typ, logicalID string) error {
	if err := s.Store.ReinstateAIResource(workspaceID, environmentID, typ, logicalID); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.localRevoke, revokeKey(workspaceID, environmentID, typ, logicalID))
	s.mu.Unlock()
	return nil
}

// CheckRevocation is the pre-execution gate for long-running tasks (§7:
// 执行前校验). Sessions frozen earlier must call this immediately before
// using a resource.
func (s *Service) CheckRevocation(workspaceID, environmentID, typ, logicalID string) (reason string, blocked bool) {
	return s.revoked(workspaceID, environmentID, typ, logicalID)
}

func revokeKey(ws, env, typ, logical string) string {
	return ws + "\x00" + env + "\x00" + typ + "\x00" + logical
}

func (s *Service) revoked(workspaceID, environmentID, typ, logicalID string) (string, bool) {
	key := revokeKey(workspaceID, environmentID, typ, logicalID)
	s.mu.RLock()
	blocked := s.localRevoke[key]
	s.mu.RUnlock()
	if blocked {
		// Store is authoritative for the reason text; a missing store
		// entry (e.g. after restart with fresh memory cache) is still
		// checked below.
		if reason, ok := s.Store.IsAIRevoked(workspaceID, environmentID, typ, logicalID); ok {
			return reason, true
		}
		s.mu.Lock()
		delete(s.localRevoke, key)
		s.mu.Unlock()
		return "", false
	}
	return s.Store.IsAIRevoked(workspaceID, environmentID, typ, logicalID)
}

// ValidateFlagModelRefs scans a flag document's variations for modelRef
// objects and enforces the workspace allowlist. Wired into the admin
// publish path so non-allowlisted models cannot be released (§7).
func (s *Service) ValidateFlagModelRefs(workspaceID string, variations map[string]any) error {
	var problems []string
	for name, val := range variations {
		obj, ok := val.(map[string]any)
		if !ok {
			continue
		}
		ref, ok := obj["modelRef"].(map[string]any)
		if !ok {
			continue
		}
		provider, _ := ref["provider"].(string)
		modelName, _ := ref["model"].(string)
		if provider == "" || modelName == "" {
			problems = append(problems, fmt.Sprintf("variation %q: modelRef needs provider and model", name))
			continue
		}
		if _, err := s.ResolveModelRef(workspaceID, provider, modelName); err != nil {
			problems = append(problems, fmt.Sprintf("variation %q: %v", name, err))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: %s", ErrModelNotAllowed, strings.Join(problems, "; "))
	}
	return nil
}
