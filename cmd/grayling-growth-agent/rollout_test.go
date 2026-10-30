package main

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/saker-ai/grayling/internal/growth"
)

func TestRolloutBinary(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "grayling-growth-agent")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, ".")
	build.Env = append(os.Environ(), "GOWORK=off")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %s %v", b, err)
	}
	for _, name := range []string{"armed", "running", "paused", "completed", "rolled_back", "absent", "missing", "denied", "redirect", "malformed response", "version", "scope response", "duplicate response", "oversized response", "unknown manifest", "duplicate manifest", "oversized manifest", "malformed reference", "scope conflict", "directory", "fifo", "timeout", "signal"} {
		t.Run(name, func(t *testing.T) {
			m := growth.RolloutManifest{WorkspaceID: "ws1", EnvironmentID: "env1", ExperimentID: "exp1", Epoch: 1, RegistrationVersion: "sha256:" + strings.Repeat("a", 64)}
			plan := map[string]any{"state": "armed", "currentStep": 0, "steps": []any{map[string]any{"name": secret, "ratio": 100, "minRuntime": 1, "autoAdvance": false}}, "createdAt": "2026-01-01T00:00:00Z", "updatedAt": "2026-01-01T00:00:00Z", "createdBy": secret, "pausedReason": secret, "history": []any{map[string]any{"at": "2026-01-01T00:00:00Z", "action": "plan.create", "step": 0, "actor": secret, "detail": secret}}}
			response := map[string]any{"id": "exp1", "workspaceId": "ws1", "environmentId": "env1", "epoch": 1, "registrationVersion": m.RegistrationVersion, "updatedAt": "2026-01-01T00:00:00Z", "releasePlan": plan}
			code := 0
			httpStatus := 200
			raw := ""
			noNetwork := false
			switch name {
			case "running", "paused", "completed", "rolled_back":
				plan["state"] = name
				plan["stepStartedAt"] = "2026-01-01T00:00:00Z"
			case "absent":
				delete(response, "releasePlan")
			case "missing":
				httpStatus = 404
				code = 3
			case "denied":
				httpStatus = 403
				code = 1
			case "redirect":
				httpStatus = 302
				code = 1
			case "malformed response", "duplicate response", "oversized response", "version", "scope response":
				code = 1
			case "unknown manifest":
				raw = `{"publish":true}`
				code = 2
				noNetwork = true
			case "duplicate manifest":
				raw = `{"epoch":1,"epoch":1}`
				code = 2
				noNetwork = true
			case "oversized manifest":
				raw = strings.Repeat(" ", growth.MaxRolloutManifestBytes+1)
				code = 2
				noNetwork = true
			case "malformed reference":
				m.ExperimentID = "../exp1"
				code = 2
				noNetwork = true
			case "scope conflict":
				m.EnvironmentID = "other"
				code = 2
				noNetwork = true
			case "directory", "fifo":
				code = 2
				noNetwork = true
			case "timeout":
				code = 1
			case "signal":
				code = 130
			}
			if name == "version" {
				response["registrationVersion"] = "sha256:" + strings.Repeat("b", 64)
			}
			if name == "scope response" {
				response["environmentId"] = "other"
			}
			var calls, writes atomic.Int32
			started := make(chan struct{}, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "GET" {
					writes.Add(1)
				}
				if r.URL.RequestURI() != "/api/v1/admin/experiments/exp1" {
					t.Errorf("unexpected path %s", r.URL)
				}
				select {
				case started <- struct{}{}:
				default:
				}
				if name == "timeout" || name == "signal" {
					<-r.Context().Done()
					return
				}
				w.Header().Set("Location", "/must-not-follow")
				w.WriteHeader(httpStatus)
				switch name {
				case "malformed response":
					_, _ = w.Write([]byte("{"))
				case "duplicate response":
					_, _ = w.Write([]byte(`{"id":"exp1","id":"exp1"}`))
				case "oversized response":
					_, _ = w.Write([]byte(strings.Repeat(" ", 2<<20+1)))
				default:
					_ = json.MarshalWrite(w, response)
				}
			}))
			defer srv.Close()
			path := filepath.Join(t.TempDir(), "manifest.json")
			if raw == "" {
				b, _ := json.Marshal(m)
				raw = string(b)
			}
			if name == "directory" {
				path = t.TempDir()
			} else if name == "fifo" {
				if err := syscall.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{"rollout", "--manifest", path, "--base-url", srv.URL, "--workspace", "ws1", "--environment", "env1"}
			if name == "timeout" {
				args = append(args, "--timeout", "30ms")
			}
			cmd := exec.CommandContext(t.Context(), binary, args...)
			cmd.Env = append(os.Environ(), "GRAYLING_GROWTH_TOKEN="+secret)
			var out, diag bytes.Buffer
			cmd.Stdout = &out
			cmd.Stderr = &diag
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			if name == "signal" {
				select {
				case <-started:
				case <-time.After(5 * time.Second):
					t.Fatal("request not started")
				}
				if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
			}
			err := cmd.Wait()
			gotCode := 0
			if err != nil {
				if e, ok := err.(*exec.ExitError); ok {
					gotCode = e.ExitCode()
				} else {
					t.Fatal(err)
				}
			}
			if gotCode != code {
				t.Fatalf("exit %d want %d; %s", gotCode, code, diag.String())
			}
			if strings.Contains(out.String()+diag.String(), secret) {
				t.Fatal("injection text leaked")
			}
			if code == 0 || code == 3 {
				var o growth.RolloutObservation
				if json.Unmarshal(out.Bytes(), &o) != nil || o.PublicationAuthorized || o.PlanPresent != (name != "absent" && name != "missing") {
					t.Fatalf("output %s", out.String())
				}
			} else if out.Len() != 0 {
				t.Fatal("partial output")
			}
			expected := int32(1)
			if noNetwork {
				expected = 0
			}
			if calls.Load() != expected || writes.Load() != 0 {
				t.Fatalf("requests %d writes %d", calls.Load(), writes.Load())
			}
		})
	}
}
