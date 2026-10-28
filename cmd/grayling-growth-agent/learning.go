package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/saker-ai/grayling/internal/growth"
)

const maxLearningInputBytes = 128 << 10

// Operator inputs are separate from proposal and evidence. JSON v2 rejects
// unknown/duplicate members, trailing values and invalid UTF-8.
func readLearningInputs(path string) (*growth.LearningInputs, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, growth.ErrLearning
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxLearningInputBytes {
		return nil, growth.ErrLearning
	}
	data, err := io.ReadAll(io.LimitReader(f, maxLearningInputBytes+1))
	if err != nil || len(data) > maxLearningInputBytes {
		return nil, growth.ErrLearning
	}
	var in *growth.LearningInputs
	if json.Unmarshal(data, &in, json.RejectUnknownMembers(true)) != nil || in == nil {
		return nil, growth.ErrLearning
	}
	return in, nil
}

func runRecordLearning(ctx context.Context, args []string, out, diagnostics io.Writer) int {
	fail := func(code int, message string) int { fmt.Fprintln(diagnostics, message); return code }
	fs := flag.NewFlagSet("record-learning", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	proposal := fs.String("proposal", "", "proposal file")
	manifest := fs.String("manifest", "", "operator manifest")
	input := fs.String("learning-input", "", "operator learning input")
	base := fs.String("base-url", "", "trusted API origin")
	workspace := fs.String("workspace", "", "trusted workspace")
	environment := fs.String("environment", "", "trusted environment")
	tokenEnv := fs.String("token-env", "GRAYLING_GROWTH_TOKEN", "credential environment variable")
	timeout := fs.Duration("timeout", 10*time.Second, "total read timeout")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(out, usage)
			return 0
		}
		return fail(2, "invalid flags; use --help")
	}
	if fs.NArg() != 0 || *proposal == "" || *manifest == "" || *input == "" || *timeout <= 0 || *timeout > 30*time.Second || !envName.MatchString(*tokenEnv) {
		return fail(2, "invalid configuration")
	}
	p, err := readProposal(*proposal)
	if err != nil {
		return fail(2, "invalid proposal file or JSON")
	}
	m, err := readMonitorManifest(*manifest)
	if err != nil {
		return fail(2, "invalid monitor manifest")
	}
	in, err := readLearningInputs(*input)
	if err != nil {
		return fail(2, "invalid learning input file or JSON")
	}
	// Classify local scope/role conflicts before the reader's evidence mismatch
	// boundary. RecordLearning validates all other local inputs before HTTP.
	if p.WorkspaceID != *workspace || p.EnvironmentID != *environment || m.WorkspaceID != *workspace || m.EnvironmentID != *environment || p.PrimaryMetric.Ref != m.PrimaryMetric || len(p.Guardrails) != len(m.Guardrails) {
		return fail(2, "learning configuration scope or metric mismatch")
	}
	guards := make(map[growth.MetricRef]bool, len(m.Guardrails))
	for _, g := range m.Guardrails {
		guards[g] = true
	}
	for _, g := range p.Guardrails {
		if !guards[g.Ref] {
			return fail(2, "learning configuration scope or metric mismatch")
		}
		delete(guards, g.Ref)
	}
	reader, err := growth.NewReader(growth.Config{BaseURL: *base, WorkspaceID: *workspace, EnvironmentID: *environment, BearerToken: os.Getenv(*tokenEnv), Timeout: *timeout})
	if err != nil {
		return fail(2, "invalid configuration")
	}
	defer reader.Close()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	record, err := reader.RecordLearning(ctx, *p, *m, *in)
	if err != nil {
		var localProblem *growth.ReviewError
		switch {
		case errors.Is(ctx.Err(), context.Canceled):
			return fail(130, "record-learning canceled")
		case errors.Is(err, growth.ErrLearning), errors.Is(err, growth.ErrProposal), errors.Is(err, growth.ErrConfig), errors.As(err, &localProblem):
			return fail(2, "invalid or stale learning input")
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			return fail(1, "evidence deadline exceeded")
		case errors.Is(err, growth.ErrAuthorization):
			return fail(1, "evidence access denied")
		case errors.Is(err, growth.ErrMismatch):
			return fail(1, "evidence scope or version mismatch")
		default:
			return fail(1, "evidence execution failed")
		}
	}
	data, err := json.Marshal(record)
	if err != nil {
		return fail(1, "output failed")
	}
	if _, err := out.Write(append(data, '\n')); err != nil {
		return fail(1, "output failed")
	}
	if record.Observation.Status != growth.Ready {
		return 3
	}
	return 0
}
