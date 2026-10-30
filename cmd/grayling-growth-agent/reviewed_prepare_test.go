package main

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/saker-ai/grayling/internal/growth"
)

func TestReviewedPrepareBinary(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "growth-agent")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, ".")
	build.Env = append(os.Environ(), "GOWORK=off")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, b)
	}
	names := append([]string{}, metricLifecycleCases...)
	names = append(names, attributionCases...)
	for _, name := range append(names, append(invalidReportMetadataCases, []string{"breach rollback", "breach continue", "breach ship", "rollback", "ship", "approve", "settings", "unhealthy", "missing evidence", "stale", "forged pin", "roles", "workspace", "reject", "request_changes", "null", "malformed", "unknown", "duplicate", "escaped duplicate", "nested unknown", "nested duplicate", "utf8", "trailing", "oversized", "boundary", "fifo", "directory", "symlink", "empty path", "missing file"}...)...) {
		t.Run(name, func(t *testing.T) {
			p := fixtureProposal(t)
			if name == "attribution overflow" {
				p.AnalysisPlan.AttributionWindowHours = 9223372036854775807
			}
			// Caller-attested fixture, deliberately without reconstructed historical
			// evidence. Fresh server evidence below remains mandatory.
			fp, err := growth.ProposalFingerprint(p)
			if err != nil {
				t.Fatal(err)
			}
			r := growth.ReviewRecord{SchemaVersion: growth.ReviewRecordSchemaVersion, ProposalID: p.ID, ProposalVersion: p.Version, WorkspaceID: p.WorkspaceID, EnvironmentID: p.EnvironmentID, ProposalFingerprint: fp, PrimaryMetric: p.PrimaryMetric, Guardrails: p.Guardrails, EvidencePins: p.EvidenceRefs, Reviewer: "operator", ReviewedAt: p.AnalysisPlan.Provenance.SuppliedAt, Decision: growth.ReviewApprove, Rationale: secret, Readiness: growth.ProposalReview{State: growth.ProposalReadyForReview, Blockers: []growth.Blocker{}}}
			want, diag := 2, "invalid or inconsistent preparation review\n"
			switch name {
			case "approve", "boundary", "rollback", "ship":
				want, diag = 0, ""
			case "settings":
				diag = "growth: preparation alpha: out_of_range\n"
			case "unhealthy", "missing evidence", "breach rollback", "breach continue", "breach ship":
				want, diag = 3, "preparation blocked: proposal not ready for review\n"
			case "stale":
				p.Rationale += " edited without version bump"
			case "forged pin":
				r.EvidencePins = append([]growth.ProposalEvidenceRef(nil), r.EvidencePins...)
				r.EvidencePins[0].Epoch++
			case "roles":
				r.PrimaryMetric, r.Guardrails = r.Guardrails[0], []growth.ProposalMetric{r.PrimaryMetric}
			case "workspace":
				r.WorkspaceID = "other"
			case "reject":
				r.Decision = growth.ReviewReject
			case "request_changes":
				r.Decision = growth.ReviewRequestChanges
			default:
				diag = "invalid review record file or JSON\n"
			}
			if strings.HasPrefix(name, "lifecycle ") {
				want, diag = 3, "preparation blocked: proposal not ready for review\n"
				if lifecycleStatus(name) == "published" {
					want, diag = 0, ""
				}
			}
			if strings.HasPrefix(name, "attribution ") {
				want, diag = 3, "preparation blocked: proposal not ready for review\n"
				if name == "attribution match" {
					want, diag = 0, ""
				}
				if name == "attribution longer guardrail" {
					want, diag = 2, "growth: preparation analysisWindowHours: shorter_than_attribution\n"
				}
			}
			if strings.HasPrefix(name, "metadata ") {
				want, diag = 3, "preparation blocked: proposal not ready for review\n"
			}
			b, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			raw := string(b)
			path := filepath.Join(t.TempDir(), "record.json")
			switch name {
			case "null":
				raw = "null"
			case "malformed":
				raw = `{"reviewer":"` + secret
			case "unknown":
				raw = `{"secret":true,` + raw[1:]
			case "duplicate":
				raw = `{"schemaVersion":"growth.review.v1",` + raw[1:]
			case "escaped duplicate":
				raw = `{"schema\u0056ersion":"growth.review.v1",` + raw[1:]
			case "nested unknown":
				raw = strings.Replace(raw, `"primaryMetric":{`, `"primaryMetric":{"secret":true,`, 1)
			case "nested duplicate":
				raw = strings.Replace(raw, `"version":1`, `"version":1,"version":1`, 1)
			case "utf8":
				raw = strings.Replace(raw, "operator", "\xff", 1)
			case "trailing":
				raw += "{}"
			case "oversized":
				raw += strings.Repeat(" ", growth.MaxReviewRecordBytes)
			case "boundary":
				raw += strings.Repeat(" ", growth.MaxReviewRecordBytes-len(raw))
			}
			if name == "fifo" {
				if err := syscall.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			} else if name == "directory" {
				path = t.TempDir()
			} else if name == "empty path" {
				path = ""
			} else if name != "missing file" {
				if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
					t.Fatal(err)
				}
				if name == "symlink" {
					link := path + ".link"
					if err := os.Symlink(path, link); err != nil {
						t.Fatal(err)
					}
					path = link
				}
			}
			var calls, writes atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				calls.Add(1)
				if req.Method != http.MethodGet {
					writes.Add(1)
				}
				if req.Header.Get("Authorization") != "Bearer "+secret {
					t.Error("missing credential")
				}
				if strings.Contains(req.URL.Path, "/metrics/") {
					key := "activation"
					if strings.Contains(req.URL.Path, "retention") {
						key = "retention"
					}
					writeLifecycleMetric(w, name, growth.Metric{WorkspaceID: "ws1", Key: key, Version: 1, Hash: p.PrimaryMetric.Ref.DefinitionHash})
					return
				}
				if name == "missing evidence" {
					w.WriteHeader(404)
					return
				}
				report := growth.Report{ExperimentID: "exp1", Epoch: 1, EvidenceVersion: p.EvidenceRefs[0].EvidenceVersion, GeneratedAt: p.AnalysisPlan.Provenance.SuppliedAt, RegistrationVersion: p.PrimaryMetric.Ref.DefinitionHash, AnalysisMethod: p.AnalysisPlan.Method, ExposureMode: "onRender", Window: p.EvidenceRefs[0].Window, Healthy: name != "unhealthy", PrimaryMetricRef: &p.PrimaryMetric.Ref, GuardrailMetricRefs: []growth.MetricRef{p.Guardrails[0].Ref}, Decision: growth.Decision{Verdict: "continue"}}
				if strings.HasPrefix(name, "breach ") {
					setGuardrailBreach(&report, strings.TrimPrefix(name, "breach "))
				} else if name == "rollback" || name == "ship" {
					report.Decision.Verdict = name
				}
				invalidateReportMetadata(name, &report)
				report.Diagnostics = &growth.Diagnostics{ExperimentID: "exp1", Epoch: 1, GeneratedAt: report.GeneratedAt, Window: report.Window}
				count, value := int64(100), 0.2
				for _, v := range p.Variants {
					report.PerVariant = append(report.PerVariant, growth.Variant{Variant: v.Key, ExposedUnits: &count, RandomizedUnits: &count, Exposures: &count, ConvertedUnits: &count, PrimaryValue: &value})
				}
				json.MarshalWrite(w, struct {
					WorkspaceID   string        `json:"workspaceId"`
					EnvironmentID string        `json:"environmentId"`
					Report        growth.Report `json:"report"`
				}{"ws1", "env1", report})
			}))
			defer srv.Close()
			settings := writeSettings(t, p)
			if name == "settings" {
				b, err := os.ReadFile(settings)
				if err != nil {
					t.Fatal(err)
				}
				b = []byte(strings.Replace(string(b), `"alpha":0.05`, `"alpha":0`, 1))
				if err := os.WriteFile(settings, b, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if name == "approve" {
				// Link a record exported by the actual binary, as well as the
				// caller-attested handcrafted boundary fixtures above.
				input := growth.ReviewInputs{ExpectedProposalFingerprint: fp, Decision: growth.ReviewApprove, Reviewer: "operator", ReviewedAt: r.ReviewedAt, Rationale: "Reviewed exact proposal."}
				b, err := json.Marshal(input)
				if err != nil {
					t.Fatal(err)
				}
				inputPath := filepath.Join(t.TempDir(), "review-input.json")
				if err := os.WriteFile(inputPath, b, 0600); err != nil {
					t.Fatal(err)
				}
				code, record, diagnostics := invokeCommand(t, binary, srv.URL, "record-review", p, "", "--review-input", inputPath)
				if code != 0 || diagnostics != "" {
					t.Fatalf("record export: %d %s", code, diagnostics)
				}
				if err := os.WriteFile(path, []byte(record), 0600); err != nil {
					t.Fatal(err)
				}
				calls.Store(0)
			}
			extra := []string{"--settings", settings, "--review-record", path}
			code, out, stderr := invokeCommand(t, binary, srv.URL, "prepare", p, "", extra...)
			if code != want || stderr != diag {
				t.Fatalf("got %d %q %q want %d %q", code, out, stderr, want, diag)
			}
			if want == 0 {
				var got growth.ReviewedPreparation
				if err := json.Unmarshal([]byte(out), &got, json.RejectUnknownMembers(true)); err != nil {
					t.Fatal(err)
				}
				if got.ReviewProvenance.Verification.PublicationAuthorized || got.ReviewProvenance.Verification.ProposalFingerprint != fp || got.ReviewProvenance.Reviewer != "operator" {
					t.Fatalf("provenance %+v", got)
				}
				_, again, d := invokeCommand(t, binary, srv.URL, "prepare", p, "", extra...)
				if out != again || d != "" {
					t.Fatal("nondeterministic output")
				}
				_, plain, d := invokeCommand(t, binary, srv.URL, "prepare", p, "", "--settings", settings)
				var unreviewed growth.PreparedExperiment
				if json.Unmarshal([]byte(plain), &unreviewed, json.RejectUnknownMembers(true)) != nil || d != "" || !reflect.DeepEqual(unreviewed, got.PreparedExperiment) {
					t.Fatalf("request changed %s %s", plain, d)
				}
			} else if out != "" {
				t.Fatal("partial output", out)
			}
			preflight := want == 2 && name != "settings" && name != "attribution longer guardrail"
			if writes.Load() != 0 || (preflight && calls.Load() != 0) || (!preflight && calls.Load() < 3) {
				t.Fatalf("calls %d writes %d", calls.Load(), writes.Load())
			}
		})
	}
}
