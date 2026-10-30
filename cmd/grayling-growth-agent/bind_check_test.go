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

func TestBindCheckBinary(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "growth")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, ".")
	build.Env = append(os.Environ(), "GOWORK=off")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build %s %v", b, err)
	}
	for _, name := range []string{"ready", "blocked", "revision drift", "snapshot drift", "registration drift", "scope drift", "denied", "check denied", "missing experiment", "missing check", "missing after", "malformed", "missing boolean", "unknown check", "aggregate", "unknown manifest", "duplicate manifest", "scope conflict", "fifo", "timeout", "signal"} {
		t.Run(name, func(t *testing.T) {
			m := growth.RolloutManifest{WorkspaceID: "ws1", EnvironmentID: "env1", ExperimentID: "exp1", Epoch: 1, RegistrationVersion: "sha256:" + strings.Repeat("a", 64)}
			code := 0
			noNetwork := false
			switch name {
			case "blocked", "revision drift", "missing experiment", "missing check", "missing after":
				code = 3
			case "snapshot drift", "registration drift", "scope drift", "denied", "check denied", "malformed", "missing boolean", "unknown check", "aggregate", "timeout":
				code = 1
			case "unknown manifest", "duplicate manifest", "scope conflict", "fifo":
				code = 2
				noNetwork = true
			case "signal":
				code = 130
			}
			var calls, writes atomic.Int32
			started := make(chan struct{}, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				if r.Method != "GET" {
					writes.Add(1)
				}
				select {
				case started <- struct{}{}:
				default:
				}
				if name == "timeout" || name == "signal" {
					<-r.Context().Done()
					return
				}
				if r.URL.Path != "/api/v1/admin/experiments/exp1" && r.URL.Path != "/api/v1/admin/experiments/exp1/bind-check" {
					t.Error("unexpected path")
				}
				if name == "denied" || (name == "check denied" && n == 2) {
					w.WriteHeader(403)
					return
				}
				if name == "missing experiment" || (name == "missing check" && n == 2) || (name == "missing after" && n == 3) {
					w.WriteHeader(404)
					return
				}
				if n == 2 {
					if name == "malformed" {
						_, _ = w.Write([]byte("{"))
						return
					}
					checks := []map[string]any{}
					for _, c := range []string{"state", "published_flag", "draft_clear", "registered_revision", "fallback", "unit", "split", "bandit_rule", "existing_binding"} {
						checks = append(checks, map[string]any{"code": c, "ready": true, "detail": secret})
					}
					ready := true
					current := 7
					switch name {
					case "blocked":
						checks[0]["ready"] = false
						ready = false
					case "revision drift":
						current = 8
						checks[3]["ready"] = false
						ready = false
					case "missing boolean":
						delete(checks[0], "ready")
					case "unknown check":
						checks[0]["code"] = secret
					case "aggregate":
						ready = false
					}
					_ = json.MarshalWrite(w, map[string]any{"experimentId": "exp1", "registeredRevision": 7, "currentRevision": current, "ready": ready, "checks": checks})
					return
				}
				snapshot := map[string]any{"id": "exp1", "workspaceId": "ws1", "environmentId": "env1", "epoch": 1, "registrationVersion": m.RegistrationVersion, "flagRevision": 7, "updatedAt": "2026-01-01T00:00:00Z"}
				if n == 3 {
					switch name {
					case "snapshot drift":
						snapshot["updatedAt"] = "2026-01-02T00:00:00Z"
					case "registration drift":
						snapshot["registrationVersion"] = "sha256:" + strings.Repeat("b", 64)
					case "scope drift":
						snapshot["environmentId"] = "other"
					}
				}
				_ = json.MarshalWrite(w, snapshot)
			}))
			defer srv.Close()
			if name == "scope conflict" {
				m.EnvironmentID = "other"
			}
			b, _ := json.Marshal(m)
			if name == "unknown manifest" {
				b = []byte(`{"publish":true}`)
			}
			if name == "duplicate manifest" {
				b = []byte(`{"epoch":1,"epoch":1}`)
			}
			file := filepath.Join(t.TempDir(), "manifest.json")
			if name == "fifo" {
				if err := syscall.Mkfifo(file, 0600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(file, b, 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{"bind-check", "--manifest", file, "--base-url", srv.URL, "--workspace", "ws1", "--environment", "env1"}
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
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
					t.Fatal("request not started")
				}
				if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
			}
			err := cmd.Wait()
			got := 0
			if err != nil {
				if e, ok := err.(*exec.ExitError); ok {
					got = e.ExitCode()
				} else {
					t.Fatal(err)
				}
			}
			if got != code {
				t.Fatalf("exit %d want %d: %s", got, code, diag.String())
			}
			if strings.Contains(out.String()+diag.String(), secret) {
				t.Fatal("secret or prose leaked")
			}
			if code == 0 || code == 3 {
				var o growth.BindingObservation
				if err := json.Unmarshal(out.Bytes(), &o); err != nil || o.Ready != (code == 0) || o.PublicationAuthorized || !o.NonAtomic || (code == 3 && len(o.Blockers) == 0) || !strings.Contains(out.String(), `"publicationAuthorized":false`) {
					t.Fatalf("invalid output %s %v", out.String(), err)
				}
			} else if out.Len() != 0 {
				t.Fatal("partial output")
			}
			if writes.Load() != 0 || (noNetwork && calls.Load() != 0) || ((code == 0 || code == 3) && name != "missing experiment" && calls.Load() != 3) {
				t.Fatalf("requests %d writes %d", calls.Load(), writes.Load())
			}
		})
	}
}
