// Package growthlab hosts an explicitly synthetic, loopback-only business
// application. It is never imported by the production server or SDK.
package growthlab

import (
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
	"time"

	gconfig "github.com/godeps/gonacos/pkg/config"
	"github.com/saker-ai/grayling/internal/api"
	"github.com/saker-ai/grayling/internal/configstore"
	"github.com/saker-ai/grayling/internal/growth"
	"github.com/saker-ai/grayling/internal/growthservice"
	"github.com/saker-ai/grayling/internal/platform"
	"github.com/saker-ai/grayling/pkg/model"
)

//go:embed ui/*
var pages embed.FS
var subjectPattern = regexp.MustCompile(`^sim-[0-9]{3}$`)
var ErrLab = errors.New("growth lab: invalid synthetic configuration or request")

type Clock struct {
	mu sync.RWMutex
	at time.Time
}

func (c *Clock) Now() time.Time { c.mu.RLock(); defer c.mu.RUnlock(); return c.at }

type State struct {
	Schema      string     `json:"schema"`
	Synthetic   bool       `json:"synthetic"`
	Workspace   string     `json:"workspace"`
	Environment string     `json:"environment"`
	At          time.Time  `json:"at"`
	Owner       string     `json:"owner"`
	Publisher   string     `json:"publisher"`
	Reader      string     `json:"reader"`
	SDK         string     `json:"sdk"`
	Collect     string     `json:"collect"`
	Operator    string     `json:"operator"`
	Editor      string     `json:"editor"`
	Reviewer    string     `json:"reviewer"`
	Viewer      string     `json:"viewer"`
	Scenarios   []Scenario `json:"scenarios"`
}
type Scenario struct {
	Key      string                   `json:"key"`
	Flag     string                   `json:"flag"`
	Primary  string                   `json:"primary"`
	Guard    string                   `json:"guard"`
	Goal     growth.GrowthGoal        `json:"goal"`
	Baseline growth.AggregateBaseline `json:"baseline"`
}
type Options struct {
	Directory, Address string
	Synthetic          bool
}
type Lab struct {
	Store                                     *platform.Store
	Core                                      *api.Server
	Growth                                    *growthservice.Service
	GrowthStore                               *growthservice.Store
	State                                     State
	Clock                                     Clock
	dir                                       string
	lock                                      *os.File
	mu                                        sync.Mutex
	sessions                                  map[string]string
	coreHTTP, growthHTTP, appHTTP             *http.Server
	coreListener, growthListener, appListener net.Listener
	cancel                                    context.CancelFunc
	done                                      chan struct{}
}

func token() string {
	var b [32]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}
func listen(address string) (net.Listener, error) {
	host, _, e := net.SplitHostPort(address)
	if e != nil {
		return nil, ErrLab
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, ErrLab
	}
	return net.Listen("tcp", address)
}
func Start(ctx context.Context, o Options) (lab *Lab, err error) {
	if !o.Synthetic || !filepath.IsAbs(o.Directory) {
		return nil, ErrLab
	}
	if e := os.MkdirAll(o.Directory, 0700); e != nil {
		return nil, e
	}
	info, e := os.Lstat(o.Directory)
	if e != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, ErrLab
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return nil, ErrLab
	}
	l := &Lab{dir: o.Directory, sessions: map[string]string{}, done: make(chan struct{})}
	defer func() {
		if err != nil {
			l.Close()
		}
	}()
	l.lock, err = os.OpenFile(filepath.Join(l.dir, "lab.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if syscall.Flock(int(l.lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return nil, ErrLab
	}
	statePath := filepath.Join(l.dir, "state.json")
	data, e := os.ReadFile(statePath)
	fresh := errors.Is(e, os.ErrNotExist)
	if !fresh {
		if e != nil || json.Unmarshal(data, &l.State, json.RejectUnknownMembers(true)) != nil || l.State.Schema != "growth.synthetic-lab.v1" || !l.State.Synthetic {
			return nil, ErrLab
		}
	}
	if fresh {
		l.Clock.at = time.Now().UTC().Truncate(time.Second)
	} else {
		l.Clock.at = l.State.At
	}
	l.Store, err = platform.NewSyntheticSQLiteStore(ctx, filepath.Join(l.dir, "platform.sqlite"), l.Clock.Now)
	if err != nil {
		return nil, err
	}
	cfg := configstore.NewGonacosStore(gconfig.NewService())
	l.Core = api.NewServer(l.Store, cfg, api.Options{Logger: log.New(io.Discard, "", 0), RequestsPerSecond: 10000})
	l.coreListener, err = listen("127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	l.coreHTTP = &http.Server{Handler: l.Core.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go l.coreHTTP.Serve(l.coreListener)
	if fresh {
		if err = l.bootstrap(ctx); err != nil {
			return nil, err
		}
	} else {
		s := l.Core.Admin.ReconcileOnceContext(ctx)
		if len(s.Errors) > 0 {
			return nil, ErrLab
		}
	}
	l.GrowthStore, err = growthservice.Open(ctx, filepath.Join(l.dir, "growth.sqlite"))
	if err != nil {
		return nil, err
	}
	l.Growth, err = growthservice.New(ctx, l.GrowthStore, growthservice.Config{Evidence: growth.Config{BaseURL: l.CoreURL(), WorkspaceID: l.State.Workspace, EnvironmentID: l.State.Environment, BearerToken: l.State.Reader}, Principals: []growthservice.Principal{{Name: "synthetic-editor", Token: l.State.Editor, Role: "editor"}, {Name: "synthetic-reviewer", Token: l.State.Reviewer, Role: "reviewer"}, {Name: "synthetic-viewer", Token: l.State.Viewer, Role: "viewer"}}, BaselineSources: []string{"synthetic-owned-export"}, Model: syntheticPlanner{}, MaxModelCalls: 1})
	if err != nil {
		return nil, err
	}
	workerCtx, cancel := context.WithCancel(ctx)
	l.cancel = cancel
	go func() { defer close(l.done); l.Growth.Run(workerCtx) }()
	l.growthListener, err = listen("127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	l.growthHTTP = &http.Server{Handler: l.Growth.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go l.growthHTTP.Serve(l.growthListener)
	address := o.Address
	if address == "" {
		address = "127.0.0.1:0"
	}
	l.appListener, err = listen(address)
	if err != nil {
		return nil, err
	}
	l.appHTTP = &http.Server{Handler: l.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go l.appHTTP.Serve(l.appListener)
	if err = l.persist(); err != nil {
		return nil, err
	}
	// Credentials stay in a private file, never stdout or participant responses.
	endpoints := map[string]string{"application": l.URL(), "platform": l.CoreURL(), "growth": l.GrowthURL()}
	raw, _ := json.Marshal(endpoints)
	if err = os.WriteFile(filepath.Join(l.dir, "endpoints.json"), raw, 0600); err != nil {
		return nil, err
	}
	return l, nil
}
func (l *Lab) URL() string       { return "http://" + l.appListener.Addr().String() }
func (l *Lab) CoreURL() string   { return "http://" + l.coreListener.Addr().String() }
func (l *Lab) GrowthURL() string { return "http://" + l.growthListener.Addr().String() }
func (l *Lab) persist() error {
	l.State.At = l.Clock.Now()
	b, e := json.Marshal(l.State, json.Deterministic(true))
	if e != nil {
		return e
	}
	tmp := filepath.Join(l.dir, "state.json.tmp")
	f, e := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if e != nil {
		return e
	}
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	if e = os.Rename(tmp, filepath.Join(l.dir, "state.json")); e != nil {
		return e
	}
	d, e := os.Open(l.dir)
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func (l *Lab) Close() {
	if l.cancel != nil {
		l.cancel()
		<-l.done
	}
	for _, s := range []*http.Server{l.appHTTP, l.growthHTTP, l.coreHTTP} {
		if s != nil {
			_ = s.Close()
		}
	}
	if l.GrowthStore != nil {
		_ = l.GrowthStore.Close()
	}
	if l.Store != nil {
		_ = l.Store.Close()
	}
	if l.lock != nil {
		_ = l.lock.Close()
	}
}
func (l *Lab) request(ctx context.Context, method, path, credential string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, e := json.Marshal(in)
		if e != nil {
			return ErrLab
		}
		body = bytes.NewReader(b)
	}
	r, e := http.NewRequestWithContext(ctx, method, l.CoreURL()+path, body)
	if e != nil {
		return ErrLab
	}
	r.Header.Set("Authorization", "Bearer "+credential)
	r.Header.Set("Content-Type", "application/json")
	c := http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer c.CloseIdleConnections()
	resp, e := c.Do(r)
	if e != nil {
		return ErrLab
	}
	defer resp.Body.Close()
	b, e := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if e != nil {
		return ErrLab
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("growth lab: platform %s status %d", path, resp.StatusCode)
	}
	if out != nil && json.Unmarshal(b, out) != nil {
		return ErrLab
	}
	return nil
}
func (l *Lab) bootstrap(ctx context.Context) error {
	ws, e := l.Store.CreateWorkspace("synthetic-growth", "Synthetic growth lab")
	if e != nil {
		return e
	}
	env, e := l.Store.CreateEnvironment(ws.ID, "local", "Local simulation")
	if e != nil {
		return e
	}
	l.State = State{Schema: "growth.synthetic-lab.v1", Synthetic: true, Workspace: ws.ID, Environment: env.ID, Operator: token(), Editor: token(), Reviewer: token(), Viewer: token()}
	for _, item := range []struct {
		kind platform.TokenKind
		role platform.Role
		env  string
		dest *string
	}{{platform.TokenAdmin, platform.RoleOwner, "", &l.State.Owner}, {platform.TokenAdmin, platform.RolePublisher, "", &l.State.Publisher}, {platform.TokenAdmin, platform.RoleViewer, "", &l.State.Reader}, {platform.TokenSDK, "", env.ID, &l.State.SDK}, {platform.TokenCollect, "", env.ID, &l.State.Collect}} {
		c, e := l.Store.CreateToken(item.kind, "synthetic-lab", item.role, ws.ID, item.env, "synthetic-fixture")
		if e != nil {
			return e
		}
		*item.dest = c.Secret
	}
	refs := map[string]growth.ProposalMetric{}
	for _, key := range []string{"first_task", "paid_upgrade", "d1_retention"} {
		spec := platform.MetricSpec{Type: "proportion", Direction: "increase"}
		events := []string{key}
		if key == "d1_retention" {
			spec.Type = "retention"
			spec.Retention = &platform.RetentionSpec{ReturnKey: "return", Day: 1, Timezone: "UTC"}
			events = []string{"return"}
		}
		m, e := l.Store.CreateMetricDefinition(&platform.MetricDefinition{WorkspaceID: ws.ID, Key: key, DisplayName: key, Owner: "synthetic-fixture", Source: "collect", EventKeys: events, DedupUnit: "user", AttributionWindow: 72 * time.Hour, Spec: spec, CreatedBy: "synthetic-fixture"})
		if e != nil {
			return e
		}
		m, e = l.Store.PublishMetricDefinition(ws.ID, platform.MetricRef{Key: key, Version: m.Version}, "synthetic-fixture")
		if e != nil {
			return e
		}
		refs[key] = growth.ProposalMetric{Ref: growth.MetricRef{Key: key, Version: m.Version, DefinitionHash: m.Hash}, SourcePath: "/api/v1/admin/workspaces/" + ws.ID + "/metrics/" + key + "/versions/1"}
	}
	for _, key := range []string{"activation", "upgrade"} {
		primary := "first_task"
		if key == "upgrade" {
			primary = "paid_upgrade"
		}
		sc := Scenario{Key: key, Flag: key + "_entry", Primary: primary, Guard: "return"}
		flag := &model.Flag{SchemaVersion: model.SchemaVersion, Key: sc.Flag, Type: model.FlagString, Fields: []string{"user_id"}, Bucketing: &model.Bucketing{Algorithm: "grayling-fnv1a-10000-v1", Unit: "user_id", Salt: "synthetic-" + key}, Variations: map[string]any{"control": "control", "treatment": "treatment"}, DefaultRule: &model.Rule{Variation: "control"}, Targeting: []model.Rule{{Percentage: map[string]float64{"control": 50, "treatment": 50}}}}
		base := "/api/v1/admin/workspaces/" + ws.ID + "/environments/" + env.ID + "/flags/" + sc.Flag
		if e = l.request(ctx, "PUT", base+"/draft", l.State.Owner, map[string]any{"content": flag, "risk": "normal", "baseRevision": 0, "expectedDraftHash": ""}, nil); e != nil {
			return e
		}
		for _, action := range []string{"submit", "approve", "publish"} {
			tok := l.State.Owner
			if action == "approve" {
				tok = l.State.Publisher
			}
			if e = l.request(ctx, "POST", base+"/"+action, tok, map[string]any{map[bool]string{true: "reason", false: "note"}[action == "submit"]: "synthetic fixture bootstrap"}, nil); e != nil {
				return e
			}
		}
		now := l.Clock.Now()
		start := now.Add(-14 * 24 * time.Hour)
		end := now.Add(-24 * time.Hour)
		sc.Goal = growth.GrowthGoal{SchemaVersion: growth.GoalSchemaVersion, ID: key, Version: 1, WorkspaceID: ws.ID, EnvironmentID: env.ID, Owner: "synthetic-fixture", Objective: "Synthetic " + key + " completion", PrimaryMetric: refs[primary], Guardrails: []growth.ProposalMetric{refs["d1_retention"]}, BaselineWindow: growth.BaselineWindow{Start: start, End: end}}
		sc.Baseline = growth.AggregateBaseline{SchemaVersion: "growth.aggregate-baseline.v1", SourceID: "synthetic-owned-export", Version: 1, WorkspaceID: ws.ID, EnvironmentID: env.ID, Metrics: []growth.MetricRef{refs[primary].Ref, refs["d1_retention"].Ref}, Window: growth.Window{Version: "aggregate-baseline-v1", Start: start, End: end, Watermark: now, Complete: true, Recomputable: true, Backend: "finite-aggregate-v1"}, RefreshedAt: now, Unit: "randomization_unit", License: "Apache-2.0 generated synthetic sample", PrivacyPolicyRef: "synthetic-no-personal-data", Synthetic: true, Groups: []growth.BaselineGroup{{Role: "all", Units: 200, Converted: 60}}}
		sampleDir := filepath.Join(l.dir, "samples")
		if e = os.MkdirAll(sampleDir, 0700); e != nil {
			return e
		}
		sample, e := json.Marshal(struct {
			Goal     growth.GrowthGoal        `json:"goal"`
			Baseline growth.AggregateBaseline `json:"baseline"`
		}{sc.Goal, sc.Baseline}, json.Deterministic(true))
		if e != nil {
			return e
		}
		if e = os.WriteFile(filepath.Join(sampleDir, sc.Key+".json"), sample, 0600); e != nil {
			return e
		}
		l.State.Scenarios = append(l.State.Scenarios, sc)
	}
	return nil
}

type syntheticPlanner struct{}

func (syntheticPlanner) Plan(_ context.Context, in growthservice.PlannerInput) ([]byte, error) {
	return json.Marshal([]growthservice.Suggestion{{Hypothesis: "Synthetic clearer entry reduces friction", Mechanism: "A visible action simplifies choice", Falsification: "Completion does not improve", Risk: "low", Effort: "small", EvidenceVersion: in.EvidenceVersion}, {Hypothesis: "Synthetic contextual guidance improves comprehension", Mechanism: "Examples clarify the next action", Falsification: "No change in completion", Risk: "low", Effort: "medium", EvidenceVersion: in.EvidenceVersion}})
}
