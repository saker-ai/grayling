package main

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/saker-ai/grayling/internal/growth"
)

type injectedOutputWriter struct {
	bytes.Buffer
	count func(int) int
	err   error
	calls int
}

func (w *injectedOutputWriter) Write(p []byte) (int, error) {
	w.calls++
	n := w.count(len(p))
	w.Buffer.Write(p[:n])
	return n, w.err
}

func checkArtifactOutput(t *testing.T, expected []byte, successCode int, run func(io.Writer, io.Writer) int) {
	t.Helper()
	if len(expected) == 0 || expected[len(expected)-1] != '\n' {
		t.Fatal("expected complete artifact including newline")
	}
	for _, tc := range []struct {
		name  string
		count func(int) int
		err   error
	}{
		{"zero nil", func(int) int { return 0 }, nil},
		{"partial nil", func(n int) int { return n / 2 }, nil},
		{"missing newline nil", func(n int) int { return n - 1 }, nil},
		{"zero error", func(int) int { return 0 }, errors.New(secret)},
		{"partial error", func(n int) int { return n / 2 }, errors.New(secret)},
		{"full error", func(n int) int { return n }, errors.New(secret)},
		{"success", func(n int) int { return n }, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := &injectedOutputWriter{count: tc.count, err: tc.err}
			var diagnostics bytes.Buffer
			wantCode, wantDiagnostic := 1, "output failed\n"
			if tc.name == "success" {
				wantCode, wantDiagnostic = successCode, ""
			}
			code := run(out, &diagnostics)
			if code != wantCode || diagnostics.String() != wantDiagnostic {
				t.Fatalf("exit %d want %d, diagnostics %q want %q", code, wantCode, diagnostics.String(), wantDiagnostic)
			}
			if out.calls != 1 || !bytes.Equal(out.Bytes(), expected[:tc.count(len(expected))]) {
				t.Fatalf("output changed or retried: calls %d, bytes %q", out.calls, out.Bytes())
			}
		})
	}
}

func TestOfflineLearningAndFingerprintOutput(t *testing.T) {
	p := fixtureProposal(t)
	r := exportedLearningFixture(t)
	p.Hypothesis = r.OriginalHypothesis
	r.ProposalFingerprint, _ = growth.ProposalFingerprint(p)
	write := func(name string, v any) string {
		t.Helper()
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	pp, rp := write("proposal.json", p), write("record.json", r)
	verification, err := growth.VerifyLearning(p, r, "ws1", "env1")
	if err != nil {
		t.Fatal(err)
	}
	search, err := growth.SearchLearning([]growth.LearningRecord{r}, growth.LearningSearchQuery{WorkspaceID: "ws1", EnvironmentID: "env1", Text: "literal"})
	if err != nil {
		t.Fatal(err)
	}
	verifiedJSON, err := json.Marshal(verification)
	if err != nil {
		t.Fatal(err)
	}
	searchJSON, err := json.Marshal(search)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		args     []string
		expected []byte
	}{
		{"fingerprint", []string{"fingerprint", "--proposal", pp}, []byte(r.ProposalFingerprint + "\n")},
		{"verify-learning", []string{"verify-learning", "--proposal", pp, "--record", rp, "--workspace", "ws1", "--environment", "env1"}, append(verifiedJSON, '\n')},
		{"search-learning", []string{"search-learning", "--record", rp, "--workspace", "ws1", "--environment", "env1", "--text", "literal"}, append(searchJSON, '\n')},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkArtifactOutput(t, tc.expected, 0, func(out, diagnostics io.Writer) int {
				return run(t.Context(), tc.args, out, diagnostics)
			})
		})
	}
}
