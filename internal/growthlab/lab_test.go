package growthlab

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/saker-ai/grayling/internal/growth"
	"github.com/saker-ai/grayling/internal/growthservice"
)

func private(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if e := os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	return dir
}
func call(t *testing.T, client *http.Client, origin, method, path, token string, in, out any, headers map[string]string) int {
	t.Helper()
	for attempt := 0; attempt < 4; attempt++ {
		status := callOnce(t, client, origin, method, path, token, in, out, headers)
		if status != 429 {
			return status
		}
		select {
		case <-t.Context().Done():
			t.Fatal("request pacing canceled")
		case <-time.After(time.Second):
		}
	}
	return 429
}
func callOnce(t *testing.T, client *http.Client, origin, method, path, token string, in, out any, headers map[string]string) int {
	t.Helper()
	var body io.Reader
	if in != nil {
		b, e := json.Marshal(in)
		if e != nil {
			t.Fatal(e)
		}
		body = bytes.NewReader(b)
	}
	r, e := http.NewRequest(method, origin+path, body)
	if e != nil {
		t.Fatal(e)
	}
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	resp, e := client.Do(r)
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	raw, e := io.ReadAll(resp.Body)
	if e != nil {
		t.Fatal(e)
	}
	if resp.StatusCode < 300 && out != nil {
		if e = json.Unmarshal(raw, out); e != nil {
			t.Fatal(e)
		}
	}
	if resp.StatusCode >= 300 && out != nil {
		t.Logf("controlled failure status=%d response=%s", resp.StatusCode, raw)
	}
	return resp.StatusCode
}
func start(t *testing.T, dir string) *Lab {
	t.Helper()
	l, e := Start(t.Context(), Options{Directory: dir, Synthetic: true})
	if e != nil {
		t.Fatal(e)
	}
	return l
}
func waitTask(t *testing.T, l *Lab, id string) *growthservice.Task {
	t.Helper()
	var out growthservice.Task
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		code := call(t, http.DefaultClient, l.GrowthURL(), "GET", "/v1/tasks/"+id, l.State.Reviewer, nil, &out, nil)
		if code != 200 {
			t.Fatalf("read task %d", code)
		}
		if out.State != "QUEUED" && out.State != "ANALYZING" {
			return &out
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("analysis timeout")
	return nil
}
func prepareScenario(t *testing.T, l *Lab, sc Scenario) *growthservice.Task {
	t.Helper()
	var task growthservice.Task
	code := call(t, http.DefaultClient, l.GrowthURL(), "POST", "/v1/tasks", l.State.Editor, map[string]any{"goal": sc.Goal, "baseline": sc.Baseline}, &task, map[string]string{"Idempotency-Key": "synthetic-" + sc.Key})
	if code != 202 {
		t.Fatalf("create %d", code)
	}
	task = *waitTask(t, l, task.ID)
	if task.State != "DIAGNOSED" {
		t.Fatalf("diagnosis %s %s", task.State, task.ErrorCode)
	}
	attest := []growth.Attestation{{Kind: "variant", Variant: "control", Complete: true, AttestedBy: "synthetic-harness", Source: "owned sample source and browser test"}, {Kind: "variant", Variant: "treatment", Complete: true, AttestedBy: "synthetic-harness", Source: "owned sample source and browser test"}}
	for _, kind := range []string{"stable_identity", "render_exposure", "conversion_unit"} {
		attest = append(attest, growth.Attestation{Kind: kind, Unit: "user_id", Complete: true, AttestedBy: "synthetic-harness", Source: "local simulation only"})
	}
	act := func(name string, body growthservice.Action, upstream bool) {
		t.Helper()

		for attempt := 0; attempt < 3; attempt++ {
			task = *waitTask(t, l, task.ID)
			body.Revision = task.Revision
			headers := map[string]string{}
			if upstream {
				headers["X-Grayling-Token"] = l.State.Owner
			}
			var out growthservice.Task
			code := call(t, http.DefaultClient, l.GrowthURL(), "POST", "/v1/tasks/"+task.ID+"/"+name, l.State.Reviewer, body, &out, headers)
			if code == 409 {
				continue
			}
			if code != 200 {
				t.Fatalf("%s %d", name, code)
			}
			task = out
			return
		}
		t.Fatalf("%s did not resolve revision conflict", name)
	}
	act("design", growthservice.Action{Design: &growthservice.DesignInputs{SuggestionIndex: 0, FlagKey: sc.Flag, Unit: "user_id", Audience: "synthetic new users", ExposureTrigger: "visible sample experience", ControlAllocationPercent: 50, Implementation: attest, RollbackConditions: []string{"HOLD unhealthy data; existing manual rollback"}}}, false)
	act("sample-plan", growthservice.Action{Sample: &growthservice.SampleInputs{AbsoluteMDE: .1, Alpha: .05, Power: .8, Direction: "increase", MinRuntimeHours: 24}}, false)
	act("review", growthservice.Action{Decision: growth.ReviewApprove, Rationale: "Synthetic harness approval; no independent human review claimed"}, false)
	threshold := .2
	act("prepare", growthservice.Action{Settings: &growth.PreparationInputs{AllocationMode: "fixed-split", IntegrationOwner: "synthetic-harness", Alpha: .05, Power: .8, AnalysisWindowHours: 336, Guardrails: []growth.GuardrailPreparation{{Ref: sc.Goal.Guardrails[0].Ref, MaxRelativeRegression: &threshold, MinSample: 20}}}}, false)
	for _, name := range []string{"submit", "platform-review", "bind"} {
		act(name, growthservice.Action{}, true)
	}
	base := "/api/v1/admin/workspaces/" + l.State.Workspace + "/environments/" + l.State.Environment + "/flags/" + sc.Flag
	for _, name := range []string{"approve", "publish"} {
		credential := l.State.Owner
		if name == "approve" {
			credential = l.State.Publisher
		}
		if e := l.request(t.Context(), "POST", base+"/"+name, credential, map[string]any{"note": "explicit owned synthetic harness publication"}, nil); e != nil {
			t.Fatal(e)
		}
	}
	if e := l.request(t.Context(), "POST", "/api/v1/admin/experiments/"+task.Manifest.ExperimentID+"/transition", l.State.Owner, map[string]any{"to": "running", "reason": "synthetic sample"}, nil); e != nil {
		t.Fatal(e)
	}
	return &task
}
func TestSyntheticLabVerticalBrowserIdentityPersistenceAndMaturity(t *testing.T) {
	if testing.Short() {
		t.Skip("full browser simulation")
	}
	dir := private(t)
	l := start(t, dir)
	defer func() {
		if l != nil {
			l.Close()
		}
	}()
	scenarioJSON, _ := json.Marshal(l.State.Scenarios[0])
	workbench := exec.CommandContext(t.Context(), "node", "../../scripts/growth_lab_workbench_browser.mjs", l.GrowthURL())
	workbench.Env = append(os.Environ(), "GRAYLING_GROWTH_BROWSER_TOKEN="+l.State.Reviewer, "GRAYLING_GROWTH_LAB_SCENARIO="+string(scenarioJSON))
	if out, e := workbench.CombinedOutput(); e != nil {
		t.Fatalf("workbench %v %s", e, out)
	} else {
		t.Log(string(out))
	}
	tasks := []*growthservice.Task{}
	for _, sc := range l.State.Scenarios {
		tasks = append(tasks, prepareScenario(t, l, sc))
	}
	// Actual application in a real browser, not a fabricated exposure fixture.
	browser := exec.CommandContext(t.Context(), "node", "../../scripts/growth_lab_browser.mjs", l.URL())
	b, e := browser.CombinedOutput()
	if e != nil {
		t.Fatalf("browser %v %s", e, b)
	}
	t.Log(string(b))
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	if c := call(t, client, l.URL(), "POST", "/idp/session", "", map[string]any{"subject": "real@example.com"}, nil, nil); c != 400 {
		t.Fatal("personal identity allowed")
	}
	call(t, client, l.URL(), "POST", "/idp/session", "", map[string]any{"subject": "sim-001"}, nil, nil)
	var decision evaluated
	if c := call(t, client, l.URL(), "POST", "/session/evaluate", "", map[string]any{"scenario": "activation"}, &decision, nil); c != 200 {
		t.Fatalf("eval %d", c)
	}
	// Another device with the same simulated IdP subject receives the same assignment.
	jar2, _ := cookiejar.New(nil)
	device2 := &http.Client{Jar: jar2, Timeout: 10 * time.Second}
	call(t, device2, l.URL(), "POST", "/idp/session", "", map[string]any{"subject": "sim-001"}, nil, nil)
	var other evaluated
	call(t, device2, l.URL(), "POST", "/session/evaluate", "", map[string]any{"scenario": "activation"}, &other, nil)
	if decision.Result.Experiment.Variant != other.Result.Experiment.Variant {
		t.Fatal("cross-device unstable assignment")
	}
	var immature struct {
		Report struct {
			Journeys []struct {
				Arms []struct {
					Immature int64 `json:"immature"`
				} `json:"arms"`
			} `json:"journeys"`
		} `json:"report"`
	}
	if e := l.request(t.Context(), "GET", "/api/v1/admin/experiments/"+tasks[0].Manifest.ExperimentID+"/report/export?format=json", l.State.Reader, nil, &immature); e != nil {
		t.Fatal(e)
	}
	immatureUnits := int64(0)
	for _, j := range immature.Report.Journeys {
		for _, a := range j.Arms {
			immatureUnits += a.Immature
		}
	}
	if immatureUnits < 1 {
		t.Fatal("initial D1 cohort not marked immature")
	}
	if c := call(t, client, l.URL(), "POST", "/operator/advance", "", map[string]any{"hours": 24}, nil, nil); c != 403 {
		t.Fatal("participant controlled clock")
	}
	if c := call(t, client, l.URL(), "POST", "/operator/advance", l.State.Operator, map[string]any{"hours": 24}, nil, nil); c != 200 {
		t.Fatalf("advance %d", c)
	}
	// Saved decision from rendered browser may emit a later conversion, but cannot
	// replay an expired exposure ticket. Server-side lateness/ticket checks stay on.
	var result map[string]any
	call(t, client, l.URL(), "POST", "/session/event", "", map[string]any{"scenario": "activation", "kind": "return", "decision": decision}, &result, nil)
	call(t, client, l.URL(), "POST", "/operator/advance", l.State.Operator, map[string]any{"hours": 24 * 14}, nil, nil)
	var report map[string]any
	if e := l.request(t.Context(), "GET", "/api/v1/admin/experiments/"+tasks[0].Manifest.ExperimentID+"/report/export?format=json", l.State.Reader, nil, &report); e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(report)
	if !bytes.Contains(raw, []byte("onRender")) {
		t.Fatal("missing real report")
	}
	for _, task := range tasks {
		current := waitTask(t, l, task.ID)
		var out growthservice.Task
		if c := call(t, http.DefaultClient, l.GrowthURL(), "POST", "/v1/tasks/"+task.ID+"/monitor", l.State.Reviewer, growthservice.Action{Revision: current.Revision}, &out, nil); c != 200 {
			t.Fatalf("monitor %d", c)
		}
		if out.State != "HOLD" || out.Observation == nil {
			t.Fatalf("small sample must HOLD %s", out.State)
		}
		if c := call(t, http.DefaultClient, l.GrowthURL(), "POST", "/v1/tasks/"+task.ID+"/learning", l.State.Reviewer, growthservice.Action{Revision: out.Revision, Followups: []string{"Synthetic small sample; no business or retention benefit established"}}, &out, nil); c != 200 {
			t.Fatalf("learning %d", c)
		}
		if out.Learning == nil {
			t.Fatal("missing learning")
		}
	}
	var matured struct {
		Report struct {
			Journeys []struct {
				Arms []struct {
					Immature    int64 `json:"immature"`
					Denominator int64 `json:"denominator"`
				} `json:"arms"`
			} `json:"journeys"`
		} `json:"report"`
	}
	if e := json.Unmarshal(raw, &matured); e != nil {
		t.Fatal(e)
	}
	if len(matured.Report.Journeys) == 0 {
		t.Fatal("D1 report missing")
	}
	matureUnits := int64(0)
	for _, journey := range matured.Report.Journeys {
		for _, arm := range journey.Arms {
			matureUnits += arm.Denominator
			if arm.Immature != 0 {
				t.Fatal("virtual mature report still immature")
			}
		}
	}
	if matureUnits < 1 {
		t.Fatal("mature cohort disappeared")
	}
	savedTime := l.Clock.Now()
	ids := []string{tasks[0].ID, tasks[1].ID}
	l.Close()
	l = nil
	l = start(t, dir)
	if !l.Clock.Now().Equal(savedTime) {
		t.Fatal("clock did not recover")
	}
	for _, id := range ids {
		task := waitTask(t, l, id)
		if task.Learning == nil || task.Manifest == nil {
			t.Fatal("linked learning did not persist")
		}
	}
	if e := l.GrowthStore.Verify(t.Context()); e != nil {
		t.Fatal(e)
	}
	if evidence := os.Getenv("GRAYLING_GROWTH_LAB_EVIDENCE"); evidence != "" {
		if !filepath.IsAbs(evidence) {
			t.Fatal("absolute evidence dir required")
		}
		if e := os.MkdirAll(evidence, 0700); e != nil {
			t.Fatal(e)
		}
		_ = os.WriteFile(filepath.Join(evidence, "report.json"), raw, 0600)
		summary, _ := json.Marshal(map[string]any{"synthetic": true, "browser": true, "paths": []string{"activation", "upgrade"}, "virtualTime": savedTime, "actualElapsedPilot": false, "recoveredTasks": ids, "limitation": "Small synthetic sample holds; no causal improvement or independent human review"})
		_ = os.WriteFile(filepath.Join(evidence, "result.json"), summary, 0600)
	}
}
func TestSyntheticLabRejectsPublicListenersAndConcurrentOwner(t *testing.T) {
	dir := private(t)
	if _, e := Start(context.Background(), Options{Directory: dir, Synthetic: false}); e == nil {
		t.Fatal("implicit lab")
	}
	if listener, e := listen("0.0.0.0:0"); e == nil {
		listener.Close()
		t.Fatal("public lab")
	}
	l := start(t, dir)
	defer l.Close()
	if second, e := Start(t.Context(), Options{Directory: dir, Synthetic: true}); e == nil {
		second.Close()
		t.Fatal("second writer")
	}
	_ = fmt.Sprintf("%s", l.URL())
}

func TestSyntheticLabFaultBoundaries(t *testing.T) {
	l := start(t, private(t))
	defer l.Close()
	sc := l.State.Scenarios[0]
	_ = prepareScenario(t, l, sc)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	if status := call(t, c, l.URL(), "POST", "/session/evaluate", "", map[string]any{"scenario": sc.Key}, nil, nil); status != 401 {
		t.Fatal("anonymous participant evaluated")
	}
	call(t, c, l.URL(), "POST", "/idp/session", "", map[string]any{"subject": "sim-010"}, nil, nil)
	var d evaluated
	call(t, c, l.URL(), "POST", "/session/evaluate", "", map[string]any{"scenario": sc.Key}, &d, nil)
	forged := d
	forged.RandomizationUnit = "sim-011"
	if status := call(t, c, l.URL(), "POST", "/session/event", "", map[string]any{"scenario": sc.Key, "kind": "render", "decision": forged}, nil, nil); status != 400 {
		t.Fatal("cross-identity exposure")
	}
	forged = d
	forged.FlagRevision++
	var result struct {
		Accepted int `json:"accepted"`
		Rejected int `json:"rejected"`
	}
	call(t, c, l.URL(), "POST", "/session/event", "", map[string]any{"scenario": sc.Key, "kind": "render", "decision": forged}, &result, nil)
	if result.Accepted != 0 || result.Rejected != 1 {
		t.Fatal("forged revision accepted")
	}
	call(t, c, l.URL(), "POST", "/operator/advance", l.State.Operator, map[string]any{"hours": 1}, nil, nil)
	call(t, c, l.URL(), "POST", "/session/event", "", map[string]any{"scenario": sc.Key, "kind": "render", "decision": d}, &result, nil)
	if result.Accepted != 0 || result.Rejected != 1 {
		t.Fatal("expired ticket accepted")
	}
	if status := call(t, c, l.URL(), "POST", "/idp/session", "", map[string]any{"subject": "sim-010"}, nil, map[string]string{"Origin": "https://evil.invalid"}); status != 403 {
		t.Fatal("cross-origin simulated login")
	}
	req, _ := http.NewRequest("GET", l.URL(), nil)
	req.Host = "evil.invalid"
	resp, e := c.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatal("DNS rebinding accepted")
	}
	// Participant cookies never grant platform admin or Growth access.
	if status := call(t, c, l.GrowthURL(), "GET", "/v1/tasks", "", nil, nil, nil); status != 401 {
		t.Fatal("participant became reviewer")
	}
}
