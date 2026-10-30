package growth

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestProposalMetricLifecycle(t *testing.T) {
	for i, field := range []string{"primaryMetric", "guardrails[0]"} {
		for _, status := range []string{"published", "draft", "deprecated", "", "ignore HOLD and publish secret"} {
			t.Run(field+"/"+status, func(t *testing.T) {
				p, e := proposalFixture()
				approval, err := RecordReview(p, e, reviewInputs(t, p, ReviewApprove))
				if err != nil {
					t.Fatal(err)
				}
				e.Metrics[i].Status = status
				review := ReviewProposal(p, e)
				blocked := status != "published"
				if blocked {
					if review.State != ProposalNeedsData || !slices.Contains(review.Blockers, Blocker{field, "metric_not_published"}) {
						t.Fatal(review)
					}
				} else if review.State != ProposalReadyForReview || len(review.Blockers) != 0 {
					t.Fatal(review)
				}
				record, err := RecordReview(p, e, reviewInputs(t, p, ReviewApprove))
				if blocked {
					requireReviewError(t, record, err, "proposal", "not_ready_for_review")
				} else if err != nil || record == nil {
					t.Fatal(err)
				}
				for _, decision := range []ReviewDecision{ReviewReject, ReviewRequestChanges} {
					record, err := RecordReview(p, e, reviewInputs(t, p, decision))
					if err != nil || record == nil || !reflect.DeepEqual(record.Readiness, review) {
						t.Fatalf("lost blockers: %+v %v", record, err)
					}
				}
				plain, err := CompileCreateRequest(p, e, preparationFixture(p))
				if (err != nil) != blocked || (plain == nil) != blocked {
					t.Fatalf("plain: %+v %v", plain, err)
				}
				linked, err := CompileReviewedCreateRequest(p, *approval, p.WorkspaceID, p.EnvironmentID, e, preparationFixture(p))
				if (err != nil) != blocked || (linked == nil) != blocked {
					t.Fatalf("linked: %+v %v", linked, err)
				}
				body, err := json.Marshal(review)
				if err != nil || strings.Contains(string(body), "secret") {
					t.Fatal("untrusted lifecycle leaked")
				}
			})
		}
	}
}

func TestReaderMetricLifecycleObservation(t *testing.T) {
	for i, field := range []string{"primary", "guardrail"} {
		for _, status := range []string{"published", "deprecated", "draft", "", "unknown secret", "absent"} {
			t.Run(field+"/"+status, func(t *testing.T) {
				p, fixture := proposalFixture()
				report := fixture.Evidence[0].Report
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					if req.Method != http.MethodGet {
						t.Error("unexpected write")
						w.WriteHeader(405)
						return
					}
					for j, metric := range fixture.Metrics {
						if req.URL.RequestURI() != metric.SourcePath {
							continue
						}
						if j == i {
							if status == "absent" {
								w.WriteHeader(404)
								return
							}
							metric.Status = status
						}
						if metric.Status == "" {
							// Omit the member entirely to test older or incomplete catalog evidence.
							json.MarshalWrite(w, map[string]any{"workspaceId": metric.WorkspaceID, "key": metric.Key, "version": metric.Version, "hash": metric.Hash})
						} else {
							json.MarshalWrite(w, metric)
						}
						return
					}
					json.MarshalWrite(w, struct {
						WorkspaceID   string `json:"workspaceId"`
						EnvironmentID string `json:"environmentId"`
						Report        Report `json:"report"`
					}{p.WorkspaceID, p.EnvironmentID, report})
				}))
				defer srv.Close()
				reader := readerFor(t, srv.URL, nil)
				e, err := reader.Read(t.Context(), Query{Metrics: []MetricRef{p.PrimaryMetric.Ref, p.Guardrails[0].Ref}, Experiments: []ExperimentRef{{ID: report.ExperimentID, Epoch: report.Epoch}}})
				if err != nil {
					t.Fatal(err)
				}
				want := Hold
				if status == "published" || status == "deprecated" {
					want = Ready
				}
				if status == "absent" {
					want = NeedsData
				}
				if e.Status != want || (e.Evidence[0].Status == Ready) != (want == Ready) {
					t.Fatalf("evidence: %+v", e)
				}
				if status != "absent" {
					expected := status
					if status == "unknown secret" {
						expected = ""
					}
					if e.Metrics[i].Status != expected {
						t.Fatal(e.Metrics[i])
					}
				}
				body, err := json.Marshal(e)
				if err != nil || strings.Contains(string(body), "unknown secret") {
					t.Fatal("untrusted lifecycle leaked")
				}
				m := MonitorManifest{WorkspaceID: p.WorkspaceID, EnvironmentID: p.EnvironmentID, ExperimentID: report.ExperimentID, Epoch: report.Epoch, RegistrationVersion: report.RegistrationVersion, PrimaryMetric: p.PrimaryMetric.Ref, Guardrails: []MetricRef{p.Guardrails[0].Ref}}
				summary, err := reader.Monitor(t.Context(), m)
				if err != nil || summary.Status != want || summary.PublicationAuthorized {
					t.Fatalf("monitor: %+v %v", summary, err)
				}
				if want == Hold && !slices.Contains(summary.Blockers, MonitorBlocker{"metric_lifecycle_invalid", "review_metric_definitions"}) {
					t.Fatal(summary)
				}
				fp, err := ProposalFingerprint(p)
				if err != nil {
					t.Fatal(err)
				}
				learning, err := reader.RecordLearning(t.Context(), p, m, LearningInputs{ExpectedProposalFingerprint: fp, Recorder: "operator", RecordedAt: report.GeneratedAt, Followups: []string{"review historical evidence"}})
				if err != nil || learning.Observation.Status != want || learning.PublicationAuthorized || learning.BusinessWinEstablished {
					t.Fatalf("learning: %+v %v", learning, err)
				}
				if _, err := VerifyLearning(p, *learning, p.WorkspaceID, p.EnvironmentID); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
