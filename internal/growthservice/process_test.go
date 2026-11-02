package growthservice

import (
	"bufio"
	"bytes"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRealProcessKillRestartAndBrowser(t *testing.T) {
	if testing.Short() {
		t.Skip("real process and browser acceptance runs in full suite")
	}
	f := newFixture(t, nil)
	f.goal.Objective = `Synthetic <img src=x onerror="window.injected=true"> first-task activation`
	binary := filepath.Join(privateDir(t), "growth-agent")
	build := exec.Command("go", "build", "-o", binary, "./cmd/grayling-growth-agent")
	build.Dir = "../.."
	build.Env = append(os.Environ(), "GOWORK=off")
	if b, e := build.CombinedOutput(); e != nil {
		t.Fatalf("build: %v %s", e, b)
	}
	modelStarted := make(chan struct{}, 1)
	releasePlanner := make(chan struct{})
	planner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case modelStarted <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-releasePlanner:
		}
	}))
	defer planner.Close()
	defer close(releasePlanner)
	private := privateDir(t)
	database := filepath.Join(private, "state.sqlite")
	config := filepath.Join(private, "service.json")
	settings := map[string]any{"database": database, "address": "127.0.0.1:0", "baseUrl": f.server.URL, "workspace": f.goal.WorkspaceID, "environment": f.goal.EnvironmentID, "readTokenEnv": "GROWTH_PROCESS_READER", "principals": []map[string]string{{"name": "reviewer", "tokenEnv": "GROWTH_PROCESS_REVIEWER", "role": "reviewer"}}, "localPlannerUrl": planner.URL}
	b, _ := json.Marshal(settings)
	if e := os.WriteFile(config, b, 0600); e != nil {
		t.Fatal(e)
	}
	start := func() (*exec.Cmd, string) {
		t.Helper()
		cmd := exec.Command(binary, "serve", "--config", config)
		cmd.Env = append(os.Environ(), "GROWTH_PROCESS_READER="+readSecret, "GROWTH_PROCESS_REVIEWER="+reviewSecret)
		stdout, e := cmd.StdoutPipe()
		if e != nil {
			t.Fatal(e)
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if e = cmd.Start(); e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() {
			if cmd.ProcessState == nil {
				cmd.Process.Kill()
				cmd.Wait()
			}
		})
		line := make(chan string, 1)
		go func() {
			scanner := bufio.NewScanner(stdout)
			if scanner.Scan() {
				line <- scanner.Text()
			} else {
				line <- ""
			}
		}()
		select {
		case text := <-line:
			if !strings.HasPrefix(text, "growth service listening on ") {
				t.Fatalf("startup: %s", text)
			}
			return cmd, "http://" + strings.TrimPrefix(text, "growth service listening on ")
		case <-time.After(20 * time.Second):
			t.Fatal("startup timeout")
		}
		return nil, ""
	}
	cmd, origin := start()
	request := func(method, path string, in any) (int, []byte) {
		t.Helper()
		var body io.Reader
		if in != nil {
			b, _ := json.Marshal(in)
			body = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, origin+path, body)
		req.Header.Set("Authorization", "Bearer "+reviewSecret)
		req.Header.Set("Idempotency-Key", "process-restart")
		req.Header.Set("Content-Type", "application/json")
		client := http.Client{Timeout: 10 * time.Second}
		resp, e := client.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer resp.Body.Close()
		b, e := io.ReadAll(resp.Body)
		if e != nil {
			t.Fatal(e)
		}
		return resp.StatusCode, b
	}
	status, b := request("POST", "/v1/tasks", map[string]any{"goal": f.goal, "manifest": f.manifest})
	if status != 202 {
		t.Fatalf("create %d %s", status, b)
	}
	task := taskFrom(t, b)
	select {
	case <-modelStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("model was not called")
	}
	if e := cmd.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	_ = cmd.Wait()
	// Budget reservation survived SIGKILL. Restart neither reruns the provider nor
	// loses the task; it completes deterministic evidence processing safely.
	cmd, origin = start()
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, b = request("GET", "/v1/tasks/"+task.ID, nil)
		task = taskFrom(t, b)
		if task.State == "HOLD" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("restart state %s", task.State)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if task.ModelCalls != 1 || task.Attempts != 2 || task.ErrorCode != "model_budget_exhausted" {
		t.Fatalf("restart budget %+v", task)
	}
	select {
	case <-modelStarted:
		t.Fatal("provider spending repeated after crash")
	default:
	}
	// Browser test runs the actual shipped UI against this real service process.
	browser := exec.Command("node", "../../scripts/growth_service_browser.mjs", origin, task.ID)
	browser.Env = append(os.Environ(), "GRAYLING_GROWTH_BROWSER_TOKEN="+reviewSecret)
	browserOut, browserErr := browser.CombinedOutput()
	if browserErr != nil {
		t.Fatalf("browser: %v %s", browserErr, browserOut)
	}
	t.Log(string(browserOut))
	if e := cmd.Process.Signal(os.Interrupt); e != nil {
		t.Fatal(e)
	}
	if e := cmd.Wait(); e != nil {
		t.Fatal(e)
	}
	backup := filepath.Join(private, "snapshot.sqlite")
	backupCommand := exec.Command(binary, "backup", "--database", database, "--output", backup)
	if raw, err := backupCommand.CombinedOutput(); err != nil {
		t.Fatalf("backup command: %v %s", err, raw)
	}
	reopened, e := Open(t.Context(), backup)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	if e = reopened.Verify(t.Context()); e != nil {
		t.Fatal(e)
	}
	t.Log("actual binary SIGKILL recovery, persisted model budget, browser review/rejection, graceful shutdown and database audit verification passed")
}
