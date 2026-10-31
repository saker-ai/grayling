// grayling-growth-agent exposes optional read-only Growth Copilot commands.
package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/saker-ai/grayling/internal/growth"
)

const usage = `Usage: grayling-growth-agent review --proposal FILE [--goal FILE] --base-url ORIGIN --workspace ID --environment ID [--token-env NAME] [--timeout 10s]
       grayling-growth-agent prepare --proposal FILE [--goal FILE] --settings FILE [--review-record FILE] --base-url ORIGIN --workspace ID --environment ID [--token-env NAME] [--timeout 10s]
       grayling-growth-agent bind-check --manifest FILE --base-url ORIGIN --workspace ID --environment ID [--token-env NAME] [--timeout 10s]
       grayling-growth-agent rollout --manifest FILE --base-url ORIGIN --workspace ID --environment ID [--token-env NAME] [--timeout 10s]
       grayling-growth-agent monitor --manifest FILE --base-url ORIGIN --workspace ID --environment ID [--token-env NAME] [--timeout 10s]
       grayling-growth-agent record-learning --proposal FILE [--goal FILE] --manifest FILE --learning-input FILE --base-url ORIGIN --workspace ID --environment ID [--token-env NAME] [--timeout 10s]
       grayling-growth-agent verify-goal --goal FILE --proposal FILE --workspace ID --environment ID
       grayling-growth-agent verify-review --proposal FILE [--goal FILE] --record FILE --workspace ID --environment ID
       grayling-growth-agent verify-learning --proposal FILE [--goal FILE] --record FILE --workspace ID --environment ID
       grayling-growth-agent search-learning --workspace ID --environment ID --text LITERAL --record FILE [--record FILE ...]
       grayling-growth-agent fingerprint --proposal FILE
       grayling-growth-agent fingerprint-goal --goal FILE
       grayling-growth-agent record-review --proposal FILE [--goal FILE] --review-input FILE --base-url ORIGIN --workspace ID --environment ID [--token-env NAME] [--timeout 10s]
--goal is required for pinned proposals; it checks caller-attested consistency before evidence reads, never approval.
verify-goal is offline: caller-attested consistency only; no baseline, readiness or approval proof.
verify-review is offline: artifact consistency only; no historical readiness proof or authorization.
verify-learning is offline: local consistency only; no authorship, execution, freshness or causality proof.
A consistent verification exits 0, including HOLD/NEEDS_DATA; publication and business win remain false.
search-learning is offline: explicit local records only, no credentials or evidence refresh.
fingerprint-goal is offline: prints validated goal content consistency hash only; no approval or baseline truth.
A valid search exits 0, including empty results and HOLD/NEEDS_DATA matches.
Read-only review/preparation; credentials come only from the named environment variable
(default GRAYLING_GROWTH_TOKEN). Timeout bounds the entire read (0 < timeout <= 30s).
prepare exports request and separate provenance metadata; it never submits.
rollout exports one pinned experiment/plan snapshot; availability is separate from release state.
bind-check is a non-atomic prerequisite observation; it never authorizes binding or publication.
monitor exports linked experiment evidence; READY never authorizes publication.
Exit codes: 0 ready_for_review/exported; 3 readiness blockers; 2 input/configuration error;
1 execution/evidence failure; 130 canceled by signal. No submission or platform approval.
record-review exports caller-attested operator content, not authenticated authorization.
`

type result struct {
	EvidenceStatus growth.Status         `json:"evidenceStatus"`
	Review         growth.ProposalReview `json:"review"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, out, diagnostics io.Writer) int {
	fail := func(code int, message string) int {
		fmt.Fprintln(diagnostics, message)
		return code
	}
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Fprint(out, usage)
		return 0
	}
	if len(args) > 0 && args[0] == "fingerprint-goal" {
		return runFingerprintGoal(args[1:], out, diagnostics)
	}
	if len(args) > 0 && args[0] == "verify-goal" {
		return runVerifyGoal(args[1:], out, diagnostics)
	}
	if len(args) > 0 && args[0] == "verify-review" {
		return runVerifyReview(args[1:], out, diagnostics)
	}
	if len(args) > 0 && args[0] == "verify-learning" {
		return runVerifyLearning(args[1:], out, diagnostics)
	}
	if len(args) > 0 && args[0] == "search-learning" {
		return runSearchLearning(args[1:], out, diagnostics)
	}
	if len(args) > 0 && args[0] == "record-learning" {
		return runRecordLearning(ctx, args[1:], out, diagnostics)
	}
	if len(args) > 0 && args[0] == "bind-check" {
		return runBindCheck(ctx, args[1:], out, diagnostics)
	}
	if len(args) > 0 && args[0] == "rollout" {
		return runRollout(ctx, args[1:], out, diagnostics)
	}
	if len(args) > 0 && args[0] == "monitor" {
		return runMonitor(ctx, args[1:], out, diagnostics)
	}
	if len(args) == 0 || (args[0] != "review" && args[0] != "prepare" && args[0] != "fingerprint" && args[0] != "record-review") {
		return fail(2, "invalid command; use --help")
	}
	prepare := args[0] == "prepare"
	fingerprintOnly := args[0] == "fingerprint"
	recordReview := args[0] == "record-review"
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	// flag errors may include caller-supplied values. Never echo them.
	fs.SetOutput(io.Discard)
	file := fs.String("proposal", "", "proposal file")
	var settingsFile, reviewRecordFile string
	if prepare {
		fs.StringVar(&reviewRecordFile, "review-record", "", "exported approval record file")
		fs.StringVar(&settingsFile, "settings", "", "operator preparation settings file")
	}
	var reviewFile string
	if recordReview {
		fs.StringVar(&reviewFile, "review-input", "", "operator review input file")
	}
	if fingerprintOnly {
		if err := parseFlags(fs, args[1:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				fmt.Fprint(out, usage)
				return 0
			}
			return fail(2, "invalid flags; use --help")
		}
		if fs.NArg() != 0 || *file == "" {
			return fail(2, "invalid configuration")
		}
		p, err := readProposal(*file)
		if err != nil {
			return fail(2, err.Error())
		}
		fp, err := growth.ProposalFingerprint(*p)
		if err != nil {
			return fail(2, "invalid proposal fingerprint input")
		}
		data := []byte(fp + "\n")
		if n, err := out.Write(data); err != nil || n != len(data) {
			return fail(1, "output failed")
		}
		return 0
	}
	goalFile := fs.String("goal", "", "optional caller-attested goal file")
	base := fs.String("base-url", "", "trusted API origin")
	workspace := fs.String("workspace", "", "trusted workspace ID")
	environment := fs.String("environment", "", "trusted environment ID")
	tokenEnv := fs.String("token-env", "GRAYLING_GROWTH_TOKEN", "credential environment variable")
	timeout := fs.Duration("timeout", 10*time.Second, "total read timeout")
	if err := parseFlags(fs, args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(out, usage)
			return 0
		}
		return fail(2, "invalid flags; use --help")
	}
	if fs.NArg() != 0 || *file == "" || *timeout <= 0 || *timeout > 30*time.Second || !envName.MatchString(*tokenEnv) {
		return fail(2, "invalid configuration")
	}
	var err error
	var settings *growth.PreparationInputs
	if prepare {
		settings, err = readSettings(settingsFile)
		if err != nil {
			return fail(2, "invalid preparation settings file or JSON")
		}
	}
	reader, err := growth.NewReader(growth.Config{BaseURL: *base, WorkspaceID: *workspace, EnvironmentID: *environment, BearerToken: os.Getenv(*tokenEnv), Timeout: *timeout})
	if err != nil {
		return fail(2, "invalid configuration")
	}
	defer reader.Close()
	p, err := readProposal(*file)
	if err != nil {
		return fail(2, err.Error())
	}
	goalSpecified := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "goal" {
			goalSpecified = true
		}
	})
	if p.GoalPin != nil && !goalSpecified {
		return fail(2, "goal file required for pinned proposal")
	}
	if goalSpecified {
		goal, err := readGoal(*goalFile)
		if err != nil {
			return fail(2, "invalid goal file or JSON")
		}
		if _, err := growth.VerifyGoal(*goal, *p, *workspace, *environment); err != nil {
			return fail(2, "invalid or inconsistent goal artifacts")
		}
	}
	var approval *growth.ReviewRecord
	if prepare {
		recordSpecified := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "review-record" {
				recordSpecified = true
			}
		})
		if recordSpecified {
			approval, err = readReviewRecord(reviewRecordFile)
			if err != nil {
				return fail(2, "invalid review record file or JSON")
			}
			if _, err = growth.VerifyPreparationReview(*p, *approval, *workspace, *environment); err != nil {
				return fail(2, "invalid or inconsistent preparation review")
			}
		}
	}
	var reviewInputs *growth.ReviewInputs
	if recordReview {
		reviewInputs, err = readReviewInputs(reviewFile)
		if err != nil {
			return fail(2, "invalid review input file or JSON")
		}
		// Reuse the pure contract to validate all operator fields and the pinned
		// fingerprint before network access. Readiness is evaluated again with
		// freshly read evidence below; no replacement fingerprint is supplied.
		_, err = growth.RecordReview(*p, nil, *reviewInputs)
		var problem *growth.ReviewError
		// A nil evidence preflight is not a completed export: fresh evidence
		// can remove missing-evidence blockers and bring it within the bound.
		if err != nil && !(errors.As(err, &problem) &&
			((problem.Field == "proposal" && problem.Code == "not_ready_for_review") ||
				(problem.Field == "record" && problem.Code == "too_large_or_invalid"))) {
			return fail(2, "invalid or stale review input")
		}
	}
	if p.WorkspaceID != *workspace || p.EnvironmentID != *environment {
		return fail(2, "proposal configuration scope mismatch")
	}
	q, err := proposalQuery(*p)
	if err != nil {
		return fail(2, "invalid proposal references")
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	envelope, err := reader.Read(ctx, q)
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return fail(130, args[0]+" canceled")
		}
		if errors.Is(err, growth.ErrConfig) {
			return fail(2, "invalid proposal references")
		}
		// Only fixed codes leave this boundary; neither server text nor URLs,
		// proposal prose, file paths or credential values are emitted.
		message := "evidence execution failed"
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			message = "evidence deadline exceeded"
		case errors.Is(err, growth.ErrAuthorization):
			message = "evidence access denied"
		case errors.Is(err, growth.ErrMismatch):
			message = "evidence scope or version mismatch"
		}
		return fail(1, message)
	}
	if recordReview {
		record, err := growth.RecordReview(*p, envelope, *reviewInputs)
		if err != nil {
			var problem *growth.ReviewError
			if errors.As(err, &problem) && problem.Field == "proposal" && problem.Code == "not_ready_for_review" {
				return fail(3, "review blocked: proposal not ready for review")
			}
			return fail(2, "invalid or stale review input")
		}
		if err := writeReviewRecord(out, record); err != nil {
			if errors.Is(err, growth.ErrReviewVerification) {
				return fail(2, "invalid or stale review input")
			}
			return fail(1, "output failed")
		}
		return 0
	}
	review := growth.ReviewProposal(*p, envelope)
	if prepare {
		if review.State != growth.ProposalReadyForReview {
			return fail(3, "preparation blocked: proposal not ready for review")
		}
		var prepared any
		if approval != nil {
			prepared, err = growth.CompileReviewedCreateRequest(*p, *approval, *workspace, *environment, envelope, *settings)
		} else {
			prepared, err = growth.CompileCreateRequest(*p, envelope, *settings)
		}
		if err != nil {
			// Compiler errors contain only bounded compiler-owned fields/codes.
			var problem *growth.PreparationError
			if errors.As(err, &problem) {
				return fail(2, problem.Error())
			}
			return fail(1, "preparation failed")
		}
		if err := writeJSONArtifact(out, prepared, json.Deterministic(approval != nil)); err != nil {
			return fail(1, "output failed")
		}
		return 0
	}
	// The library names missing variant attestations by caller-supplied key.
	// Use array positions here so even field paths cannot echo proposal secrets.
	for i := range review.Blockers {
		if strings.HasPrefix(review.Blockers[i].Field, "implementation.variant.") {
			for j, v := range p.Variants {
				if review.Blockers[i].Field == "implementation.variant."+v.Key {
					review.Blockers[i].Field = "implementation.variants[" + strconv.Itoa(j) + "]"
					break
				}
			}
		}
	}
	if err := writeJSONArtifact(out, result{EvidenceStatus: envelope.Status, Review: review}); err != nil {
		return fail(1, "output failed")
	}
	if review.State != growth.ProposalReadyForReview {
		return 3
	}
	return 0
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var pinnedHash = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Source paths are assertions, never destinations. Reader validates every
// identifier, version, hash, duplicate and count before making any request.
func proposalQuery(p growth.Proposal) (growth.Query, error) {
	q := growth.Query{}
	for _, m := range append([]growth.ProposalMetric{p.PrimaryMetric}, p.Guardrails...) {
		path := "/api/v1/admin/workspaces/" + p.WorkspaceID + "/metrics/" + m.Ref.Key + "/versions/" + strconv.FormatInt(m.Ref.Version, 10)
		if m.SourcePath != path {
			return q, growth.ErrConfig
		}
		q.Metrics = append(q.Metrics, m.Ref)
	}
	for _, e := range p.EvidenceRefs {
		if e.SourcePath != "/api/v1/admin/experiments/"+e.ExperimentID+"/report/export?format=json" || !pinnedHash.MatchString(e.EvidenceVersion) {
			return q, growth.ErrConfig
		}
		q.Experiments = append(q.Experiments, growth.ExperimentRef{ID: e.ExperimentID, Epoch: e.Epoch, EvidenceVersion: e.EvidenceVersion})
	}
	return q, nil
}

// Nonblocking open and regular-file checks exclude pipes/devices. DecodeProposal
// also bounds the actual read, including files which grow after Stat.
func readProposal(path string) (*growth.Proposal, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.New("cannot read proposal file")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > growth.MaxProposalBytes {
		return nil, errors.New("invalid proposal file")
	}
	p, err := growth.DecodeProposal(f)
	if err != nil {
		return nil, errors.New("invalid proposal JSON")
	}
	return p, nil
}

// writeReviewRecord buffers the entire file before output. The consumer's byte
// bound includes JSON escaping and the trailing newline.
func writeReviewRecord(out io.Writer, record *growth.ReviewRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if len(data)+1 > growth.MaxReviewRecordBytes {
		return growth.ErrReviewVerification
	}
	data = append(data, '\n')
	n, err := out.Write(data)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}

// writeJSONArtifact serializes the complete artifact before a single write so
// even truncation of only the trailing newline is an output failure.
func writeJSONArtifact(out io.Writer, value any, options ...json.Options) error {
	data, err := json.Marshal(value, options...)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	n, err := out.Write(data)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}
