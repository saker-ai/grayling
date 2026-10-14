package rollout_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	gconfig "github.com/godeps/gonacos/pkg/config"
	"github.com/saker-ai/grayling/internal/api"
	"github.com/saker-ai/grayling/internal/configstore"
	"github.com/saker-ai/grayling/internal/platform"
	"github.com/saker-ai/grayling/test/pgtest"
)

// The helper runs the same HTTP stack in a distinct OS process. The parent
// starts it after preparing PostgreSQL, so its store begins with a separate
// in-memory snapshot that must be refreshed under the plan lock.
func TestRolloutReplicaProcessHelper(t *testing.T) {
	dsn := os.Getenv("GRAYLING_ROLLOUT_REPLICA_DSN")
	if dsn == "" {
		t.Skip("helper process only")
	}
	store, err := platform.NewPGStore(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := api.NewServer(store, configstore.NewGonacosStore(gconfig.NewService()), api.Options{})
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	fmt.Fprintln(os.Stdout, httpServer.URL)
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestTwoProcessPlanConflictAndReportEvidence(t *testing.T) {
	dsn := pgtest.DSN(t)
	store, err := platform.NewPGStore(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	h := newHarnessOnStore(t, store)
	expID := h.runningExperiment(t, "process-report", nil)
	h.createPlan(t, expID,
		step{Name: "canary", Ratio: 20, MinRuntime: int64(time.Hour)},
		step{Name: "full", Ratio: 100, MinRuntime: int64(time.Hour)})
	h.tick()
	openedVersion := h.reportVersion(t, expID)

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestRolloutReplicaProcessHelper$")
	cmd.Env = append(os.Environ(), "GRAYLING_ROLLOUT_REPLICA_DSN="+dsn)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	type readyResult struct {
		address string
		err     error
	}
	ready := make(chan readyResult, 1)
	go func() {
		address, err := bufio.NewReader(stdout).ReadString('\n')
		ready <- readyResult{address, err}
	}()
	var address string
	select {
	case result := <-ready:
		address, err = result.address, result.err
	case <-time.After(10 * time.Second):
		t.Fatal("second process did not start within 10 seconds")
	}
	if err != nil || !strings.HasPrefix(address, "http://") {
		t.Fatalf("second process did not start: address=%q err=%v", address, err)
	}
	address = strings.TrimSpace(address)
	client := &http.Client{Timeout: 10 * time.Second}
	post := func(base, path string, body any) (int, string, error) {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, "", err
		}
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+path, bytes.NewReader(data))
		if err != nil {
			return 0, "", err
		}
		request.Header.Set("Authorization", "Bearer "+h.owner)
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		if err != nil {
			return 0, "", err
		}
		defer response.Body.Close()
		responseBody, err := io.ReadAll(response.Body)
		return response.StatusCode, string(responseBody), err
	}
	base := "/api/v1/admin/experiments/" + expID + "/release-plan"
	type result struct {
		status int
		body   string
		err    error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for _, serverURL := range []string{h.srv.URL, address} {
		go func() {
			<-start
			status, body, err := post(serverURL, base+"/pause", map[string]any{"reason": "incident"})
			results <- result{status, body, err}
		}()
	}
	close(start)
	first, second := <-results, <-results
	if first.err != nil || second.err != nil || !((first.status == http.StatusOK && second.status == http.StatusConflict) ||
		(first.status == http.StatusConflict && second.status == http.StatusOK)) {
		t.Fatalf("two-process pause: %+v %+v", first, second)
	}
	if err := store.ReloadFromBackend(); err != nil {
		t.Fatal(err)
	}
	plan := h.getPlan(t, expID)
	pauses := 0
	for _, event := range plan.History {
		if event.Action == "manual.pause" {
			pauses++
		}
	}
	if pauses != 1 || plan.State != platform.ReleasePlanPaused {
		t.Fatalf("concurrent pause changed plan more than once: %+v", plan)
	}
	pauseAudit := 0
	for _, entry := range store.AuditList(h.ws.ID, 100) {
		if entry.Action == "release_plan.pause" && entry.Object == expID {
			pauseAudit++
		}
	}
	if pauseAudit != 1 {
		t.Fatalf("concurrent pause audit count=%d", pauseAudit)
	}

	h.feedTraffic(t, expID, 0, 200, 0.3, 0.1, nil)
	h.fakeNow = time.Now().UTC()
	currentVersion := h.reportVersion(t, expID)
	if currentVersion == openedVersion {
		t.Fatal("new events did not change report evidence")
	}
	status, body, err := post(address, base+"/resume", map[string]any{"expectedReportVersion": openedVersion})
	if err != nil || status != http.StatusConflict {
		t.Fatalf("stale evidence across process: status=%d body=%s err=%v", status, body, err)
	}
	if err := store.ReloadFromBackend(); err != nil {
		t.Fatal(err)
	}
	if plan = h.getPlan(t, expID); plan.State != platform.ReleasePlanPaused {
		t.Fatalf("stale evidence changed plan: %+v", plan)
	}
	status, body, err = post(address, base+"/resume", map[string]any{"expectedReportVersion": currentVersion})
	if err != nil || status != http.StatusOK {
		t.Fatalf("current evidence across process: status=%d opened=%s current=%s body=%s err=%v", status, openedVersion, currentVersion, body, err)
	}
	if err := store.ReloadFromBackend(); err != nil {
		t.Fatal(err)
	}
	plan = h.getPlan(t, expID)
	if plan.State != platform.ReleasePlanRunning {
		t.Fatalf("resume did not persist across process: %+v", plan)
	}
	resumeAudit := 0
	for _, entry := range store.AuditList(h.ws.ID, 100) {
		if entry.Action == "release_plan.resume" && entry.Object == expID &&
			strings.Contains(entry.Detail, currentVersion) {
			resumeAudit++
		}
	}
	if resumeAudit != 1 {
		t.Fatalf("current evidence audit count=%d", resumeAudit)
	}

	status, body, err = post(h.srv.URL, base+"/takeover", map[string]any{
		"enabled": true, "reason": "operator hold",
	})
	if err != nil || status != http.StatusOK {
		t.Fatalf("enable takeover: status=%d body=%s err=%v", status, body, err)
	}
	openedVersion = h.reportVersion(t, expID)
	h.feedTraffic(t, expID, 200, 100, 0.3, 0.1, nil)
	h.fakeNow = time.Now().UTC()
	currentVersion = h.reportVersion(t, expID)
	if currentVersion == openedVersion {
		t.Fatal("second event batch did not change report evidence")
	}
	status, body, err = post(address, base+"/takeover", map[string]any{
		"enabled": false, "expectedReportVersion": openedVersion,
	})
	if err != nil || status != http.StatusConflict {
		t.Fatalf("stale takeover return: status=%d body=%s err=%v", status, body, err)
	}
	if err := store.ReloadFromBackend(); err != nil {
		t.Fatal(err)
	}
	if plan = h.getPlan(t, expID); !plan.ManualOverride {
		t.Fatalf("stale evidence released manual control: %+v", plan)
	}
	status, body, err = post(address, base+"/takeover", map[string]any{
		"enabled": false, "expectedReportVersion": currentVersion,
	})
	if err != nil || status != http.StatusOK {
		t.Fatalf("current takeover return: status=%d body=%s err=%v", status, body, err)
	}
	if err := store.ReloadFromBackend(); err != nil {
		t.Fatal(err)
	}
	if plan = h.getPlan(t, expID); plan.ManualOverride {
		t.Fatalf("current evidence did not return automatic control: %+v", plan)
	}
	takeoverVersion := currentVersion

	if code := h.do(http.MethodPost, base+"/advance", h.owner, map[string]any{
		"reason": "move to final step", "expectedReportVersion": h.reportVersion(t, expID),
	}, nil); code != http.StatusOK {
		t.Fatalf("first advance: status=%d", code)
	}
	openedVersion = h.reportVersion(t, expID)
	h.feedTraffic(t, expID, 300, 100, 0.3, 0.1, nil)
	h.fakeNow = time.Now().UTC()
	currentVersion = h.reportVersion(t, expID)
	if currentVersion == openedVersion {
		t.Fatal("third event batch did not change report evidence")
	}
	status, body, err = post(address, base+"/advance", map[string]any{
		"reason": "old evidence", "expectedReportVersion": openedVersion,
	})
	if err != nil || status != http.StatusConflict {
		t.Fatalf("stale final advance: status=%d body=%s err=%v", status, body, err)
	}
	if err := store.ReloadFromBackend(); err != nil {
		t.Fatal(err)
	}
	if plan = h.getPlan(t, expID); plan.CurrentStep != 1 || plan.State != platform.ReleasePlanRunning {
		t.Fatalf("stale evidence advanced plan: %+v", plan)
	}
	status, body, err = post(address, base+"/advance", map[string]any{
		"reason": "finish rollout", "expectedReportVersion": currentVersion,
	})
	if err != nil || status != http.StatusOK {
		t.Fatalf("current final advance: status=%d body=%s err=%v", status, body, err)
	}
	if err := store.ReloadFromBackend(); err != nil {
		t.Fatal(err)
	}
	if plan = h.getPlan(t, expID); plan.State != platform.ReleasePlanCompleted {
		t.Fatalf("current evidence did not complete plan: %+v", plan)
	}
	for _, action := range []string{"release_plan.takeover", "release_plan.manual_complete"} {
		version := currentVersion
		if action == "release_plan.takeover" {
			version = takeoverVersion
		}
		found := false
		for _, entry := range store.AuditList(h.ws.ID, 100) {
			if entry.Action == action && entry.Object == expID &&
				strings.Contains(entry.Detail, version) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s lacks checked report evidence version", action)
		}
	}
}
