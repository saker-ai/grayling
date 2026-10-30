package growth

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func bindingFixture() map[string]any {
	checks := []any{}
	for _, p := range bindingPrerequisites {
		checks = append(checks, map[string]any{"code": p.Code, "ready": true, "detail": "secret publish now"})
	}
	return map[string]any{"experimentId": "exp1", "registeredRevision": 7, "currentRevision": 7, "ready": true, "checks": checks}
}
func TestBindCheckHTTP(t *testing.T) {
	for _, name := range []string{"ready", "blocked", "revision drift", "no flag", "missing experiment", "missing check", "missing after", "denied", "unauthorized", "check denied", "after denied", "changed time", "changed revision", "changed scope", "changed registration", "changed epoch", "id", "registered", "negative revision", "missing revision", "null revision", "missing ready", "null ready", "missing check ready", "null check ready", "missing checks", "duplicate check", "unknown check", "aggregate", "revision consistency", "malformed", "null", "duplicate JSON", "redirect", "oversized", "snapshot revision absent", "snapshot time absent", "scope", "cancel"} {
		t.Run(name, func(t *testing.T) {
			b := bindingFixture()
			snapshot := rolloutResponseFixture()
			snapshot["flagRevision"] = 7
			checks := b["checks"].([]any)
			want := error(nil)
			ready := true
			switch name {
			case "blocked":
				checks[0].(map[string]any)["ready"] = false
				b["ready"] = false
				ready = false
			case "revision drift":
				b["currentRevision"] = 8
				checks[3].(map[string]any)["ready"] = false
				b["ready"] = false
				ready = false
			case "no flag":
				b["currentRevision"] = 0
				b["ready"] = false
				ready = false
				for _, c := range checks {
					c.(map[string]any)["ready"] = false
				}
			case "missing experiment", "missing check", "missing after":
				ready = false
			case "denied", "unauthorized", "check denied", "after denied":
				want = ErrAuthorization
			case "changed time", "changed revision", "changed scope", "changed registration", "changed epoch", "id", "registered", "scope":
				want = ErrMismatch
			case "cancel":
				want = context.Canceled
			case "ready":
			default:
				want = ErrResponse
			}
			switch name {
			case "id":
				b["experimentId"] = "other"
			case "registered":
				b["registeredRevision"] = 8
			case "negative revision":
				b["currentRevision"] = -1
			case "missing revision":
				delete(b, "currentRevision")
			case "null revision":
				b["registeredRevision"] = nil
			case "missing ready":
				delete(b, "ready")
			case "null ready":
				b["ready"] = nil
			case "missing check ready":
				delete(checks[0].(map[string]any), "ready")
			case "null check ready":
				checks[0].(map[string]any)["ready"] = nil
			case "missing checks":
				b["checks"] = checks[:8]
			case "duplicate check":
				checks[1] = checks[0]
			case "unknown check":
				checks[0].(map[string]any)["code"] = "secret"
			case "aggregate":
				b["ready"] = false
			case "revision consistency":
				b["currentRevision"] = 8
			case "snapshot revision absent":
				delete(snapshot, "flagRevision")
			case "snapshot time absent":
				delete(snapshot, "updatedAt")
			}
			calls, writes := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "GET" {
					writes++
				}
				if r.Header.Get("Authorization") != "Bearer secret" {
					t.Error("credential missing")
				}
				isCheck := strings.HasSuffix(r.URL.Path, "/bind-check")
				if (calls == 2) != isCheck {
					t.Error("incorrect read order")
				}
				if r.URL.Path != "/api/v1/admin/experiments/exp1" && !isCheck {
					t.Error("incorrect path")
				}
				if name == "missing experiment" || (name == "missing check" && isCheck) || (name == "missing after" && calls == 3) {
					w.WriteHeader(404)
					return
				}
				if name == "denied" || (name == "check denied" && isCheck) || (name == "after denied" && calls == 3) {
					w.WriteHeader(403)
					return
				}
				if name == "unauthorized" {
					w.WriteHeader(401)
					return
				}
				if isCheck {
					switch name {
					case "malformed":
						_, _ = w.Write([]byte("{"))
						return
					case "null":
						_, _ = w.Write([]byte("null"))
						return
					case "duplicate JSON":
						_, _ = w.Write([]byte(`{"ready":true,"ready":true}`))
						return
					case "redirect":
						w.Header().Set("Location", "/write")
						w.WriteHeader(302)
						return
					case "oversized":
						_, _ = w.Write([]byte(strings.Repeat(" ", 2<<20+1)))
						return
					}
					_ = json.MarshalWrite(w, b)
					return
				}
				if calls == 3 {
					switch name {
					case "changed time":
						snapshot["updatedAt"] = "2026-01-02T00:00:00Z"
					case "changed revision":
						snapshot["flagRevision"] = 8
					case "changed scope":
						snapshot["environmentId"] = "other"
					case "changed registration":
						snapshot["registrationVersion"] = hashB
					case "changed epoch":
						snapshot["epoch"] = 2
					}
				}
				_ = json.MarshalWrite(w, snapshot)
			}))
			defer srv.Close()
			r, err := NewReader(Config{BaseURL: srv.URL, WorkspaceID: "ws1", EnvironmentID: "env1", BearerToken: "secret"})
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			m := rolloutManifestFixture()
			if name == "scope" {
				m.WorkspaceID = "other"
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if name == "cancel" {
				cancel()
			}
			o, err := r.BindCheck(ctx, m)
			if want != nil {
				if o != nil || !errors.Is(err, want) {
					t.Fatalf("%+v %v; want %v", o, err, want)
				}
			} else {
				if err != nil || o == nil || o.Ready != ready || o.PublicationAuthorized || !o.NonAtomic || (!ready && len(o.Blockers) == 0) {
					t.Fatalf("%+v %v", o, err)
				}
				data, _ := json.Marshal(o)
				if strings.Contains(string(data), "secret") {
					t.Fatal("server prose leaked")
				}
			}
			if writes != 0 || calls > 3 || ((name == "cancel" || name == "scope") && calls != 0) || (want == nil && name != "missing experiment" && calls != 3) {
				t.Fatalf("requests %d writes %d", calls, writes)
			}
		})
	}
}
