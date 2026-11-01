package growthlab

import (
	"bufio"
	"encoding/json/v2"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type remoteListener struct {
	net.Listener
	address net.Addr
}

func (r remoteListener) Addr() net.Addr { return r.address }

// This process acceptance kills the actual standalone lab while both SQLite
// stores contain real API registrations and browser-collected events.
func TestSyntheticLabProcessKillAndRecover(t *testing.T) {
	if testing.Short() {
		t.Skip("standalone process and browser")
	}
	dir := private(t)
	binary := filepath.Join(private(t), "growth-lab")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "./cmd/grayling-growth-lab")
	build.Dir = "../.."
	if b, e := build.CombinedOutput(); e != nil {
		t.Fatalf("build %v %s", e, b)
	}
	startProcess := func() (*exec.Cmd, *Lab) {
		t.Helper()
		cmd := exec.Command(binary, "--synthetic", "--data-dir", dir)
		stdout, e := cmd.StdoutPipe()
		if e != nil {
			t.Fatal(e)
		}
		if e = cmd.Start(); e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() {
			if cmd.ProcessState == nil {
				cmd.Process.Kill()
				cmd.Wait()
			}
		})
		ready := make(chan bool, 1)
		go func() {
			scanner := bufio.NewScanner(stdout)
			ready <- scanner.Scan() && strings.HasPrefix(scanner.Text(), "synthetic lab listening on http://127.0.0.1:")
		}()
		select {
		case ok := <-ready:
			if !ok {
				t.Fatal("standalone startup")
			}
		case <-time.After(30 * time.Second):
			t.Fatal("standalone startup timeout")
		}
		stateRaw, e := os.ReadFile(filepath.Join(dir, "state.json"))
		if e != nil {
			t.Fatal(e)
		}
		var state State
		if json.Unmarshal(stateRaw, &state) != nil {
			t.Fatal("private state decode")
		}
		raw, e := os.ReadFile(filepath.Join(dir, "endpoints.json"))
		if e != nil {
			t.Fatal(e)
		}
		var endpoints map[string]string
		if json.Unmarshal(raw, &endpoints) != nil {
			t.Fatal("private endpoint decode")
		}
		listener := func(key string) net.Listener {
			address, e := net.ResolveTCPAddr("tcp", strings.TrimPrefix(endpoints[key], "http://"))
			if e != nil {
				t.Fatal(e)
			}
			return remoteListener{address: address}
		}
		return cmd, &Lab{State: state, appListener: listener("application"), coreListener: listener("platform"), growthListener: listener("growth")}
	}
	cmd, l := startProcess()
	tasks := []string{}
	for _, sc := range l.State.Scenarios {
		tasks = append(tasks, prepareScenario(t, l, sc).ID)
	}
	browser := exec.CommandContext(t.Context(), "node", "../../scripts/growth_lab_browser.mjs", l.URL())
	if b, e := browser.CombinedOutput(); e != nil {
		t.Fatalf("standalone browser %v %s", e, b)
	} else {
		t.Log(string(b))
	}
	if e := cmd.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	_ = cmd.Wait()
	cmd, l = startProcess()
	for _, id := range tasks {
		task := waitTask(t, l, id)
		if task.Manifest == nil || !task.SubmissionConfirmed {
			t.Fatal("registered experiment lost after SIGKILL")
		}
		var report struct {
			Report struct {
				PerVariant []struct {
					ExposedUnits int64 `json:"exposedUnits"`
				} `json:"perVariant"`
			} `json:"report"`
		}
		if e := l.request(t.Context(), "GET", "/api/v1/admin/experiments/"+task.Manifest.ExperimentID+"/report/export?format=json", l.State.Reader, nil, &report); e != nil {
			t.Fatal(e)
		}
		units := int64(0)
		for _, v := range report.Report.PerVariant {
			units += v.ExposedUnits
		}
		if units != 1 {
			t.Fatalf("browser exposure lost or duplicated after SIGKILL: %d", units)
		}
	}
	if e := cmd.Process.Signal(os.Interrupt); e != nil {
		t.Fatal(e)
	}
	if e := cmd.Wait(); e != nil {
		t.Fatal("standalone graceful shutdown")
	}
}
