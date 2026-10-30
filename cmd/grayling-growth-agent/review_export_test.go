package main

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/saker-ai/grayling/internal/growth"
)

type reviewShortWriter struct{ calls int }

func (w *reviewShortWriter) Write(p []byte) (int, error) { w.calls++; return len(p) - 1, nil }

func TestWriteReviewRecordSerializedBoundary(t *testing.T) {
	for _, target := range []int{growth.MaxReviewRecordBytes - 1, growth.MaxReviewRecordBytes, growth.MaxReviewRecordBytes + 1} {
		p := fixtureProposal(t)
		fp, err := growth.ProposalFingerprint(p)
		if err != nil {
			t.Fatal(err)
		}
		r, err := growth.RecordReview(p, nil, growth.ReviewInputs{ExpectedProposalFingerprint: fp, Decision: growth.ReviewReject, Reviewer: "operator", ReviewedAt: p.AnalysisPlan.Provenance.SuppliedAt, Rationale: "r" + strings.Repeat("\t", growth.MaxReviewRationaleBytes-1)})
		if err != nil {
			t.Fatal(err)
		}
		r.EvidencePins[0].Window.Issues = []string{""}
		data, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		r.EvidencePins[0].Window.Issues[0] = strings.Repeat("a", target-len(data)-2) + "\t"
		data, err = json.Marshal(r)
		if err != nil || len(data) != target {
			t.Fatalf("setup: %d %v", len(data), err)
		}
		var out bytes.Buffer
		err = writeReviewRecord(&out, r)
		if target == growth.MaxReviewRecordBytes-1 {
			if err != nil || out.Len() != growth.MaxReviewRecordBytes || out.Bytes()[out.Len()-1] != '\n' {
				t.Fatalf("bounded output: %d %v", out.Len(), err)
			}
			if _, err := growth.DecodeReviewRecord(bytes.NewReader(out.Bytes())); err != nil {
				t.Fatal(err)
			}
			short := &reviewShortWriter{}
			if err := writeReviewRecord(short, r); !errors.Is(err, io.ErrShortWrite) || short.calls != 1 {
				t.Fatalf("short write: %v", err)
			}
		} else if !errors.Is(err, growth.ErrReviewVerification) || out.Len() != 0 {
			t.Fatalf("unbounded output: %d %v", out.Len(), err)
		}
	}
}
