// grayling-growth-agent exposes optional read-only proposal review and preparation.
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

const usage = `Usage: grayling-growth-agent review --proposal FILE --base-url ORIGIN --workspace ID --environment ID [--token-env NAME] [--timeout 10s]
       grayling-growth-agent prepare --proposal FILE --settings FILE --base-url ORIGIN --workspace ID --environment ID [--token-env NAME] [--timeout 10s]
Read-only review/preparation; credentials come only from the named environment variable
(default GRAYLING_GROWTH_TOKEN). Timeout bounds the entire read (0 < timeout <= 30s).
prepare exports request and separate provenance metadata; it never submits.
Exit codes: 0 ready_for_review/exported; 3 readiness blockers; 2 input/configuration error;
1 execution/evidence failure; 130 canceled by signal. No approval or submission.
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
	if len(args) == 0 || (args[0] != "review" && args[0] != "prepare") {
		return fail(2, "invalid command; use --help")
	}
	prepare := args[0] == "prepare"
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	// flag errors may include caller-supplied values. Never echo them.
	fs.SetOutput(io.Discard)
	file := fs.String("proposal", "", "proposal file")
	var settingsFile string
	if prepare {
		fs.StringVar(&settingsFile, "settings", "", "operator preparation settings file")
	}
	base := fs.String("base-url", "", "trusted API origin")
	workspace := fs.String("workspace", "", "trusted workspace ID")
	environment := fs.String("environment", "", "trusted environment ID")
	tokenEnv := fs.String("token-env", "GRAYLING_GROWTH_TOKEN", "credential environment variable")
	timeout := fs.Duration("timeout", 10*time.Second, "total read timeout")
	if err := fs.Parse(args[1:]); err != nil {
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
	// Nonblocking open plus regular-file check excludes pipes/devices and keeps
	// proposal input bounded without leaving a blocked input goroutine behind.
	f, err := os.OpenFile(*file, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fail(2, "cannot read proposal file")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > growth.MaxProposalBytes {
		return fail(2, "invalid proposal file")
	}
	p, err := growth.DecodeProposal(f)
	if err != nil {
		return fail(2, "invalid proposal JSON")
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
	review := growth.ReviewProposal(*p, envelope)
	if prepare {
		if review.State != growth.ProposalReadyForReview {
			return fail(3, "preparation blocked: proposal not ready for review")
		}
		prepared, err := growth.CompileCreateRequest(*p, envelope, *settings)
		if err != nil {
			// Compiler errors contain only bounded compiler-owned fields/codes.
			var problem *growth.PreparationError
			if errors.As(err, &problem) {
				return fail(2, problem.Error())
			}
			return fail(1, "preparation failed")
		}
		if err := json.MarshalWrite(out, prepared); err != nil {
			return fail(1, "output failed")
		}
		if _, err := fmt.Fprintln(out); err != nil {
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
	if err := json.MarshalWrite(out, result{EvidenceStatus: envelope.Status, Review: review}); err != nil {
		return fail(1, "output failed")
	}
	if _, err := fmt.Fprintln(out); err != nil {
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
