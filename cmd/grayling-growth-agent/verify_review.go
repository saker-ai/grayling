package main

import (
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/saker-ai/grayling/internal/growth"
)

func runVerifyReview(args []string, out, diagnostics io.Writer) int {
	fail := func(message string) int { fmt.Fprintln(diagnostics, message); return 2 }
	fs := flag.NewFlagSet("verify-review", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	goalFile := fs.String("goal", "", "optional caller-attested goal file")
	proposal := fs.String("proposal", "", "original proposal file")
	record := fs.String("record", "", "exported review record file")
	workspace := fs.String("workspace", "", "required workspace")
	environment := fs.String("environment", "", "required environment")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(out, usage)
			return 0
		}
		return fail("invalid flags; use --help")
	}
	if fs.NArg() != 0 || *proposal == "" || *record == "" || *workspace == "" || *environment == "" {
		return fail("invalid verification configuration")
	}
	p, err := readProposal(*proposal)
	if err != nil {
		return fail("invalid proposal file or JSON")
	}
	goalSpecified := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "goal" {
			goalSpecified = true
		}
	})
	if p.GoalPin != nil && !goalSpecified {
		return fail("goal file required for pinned proposal")
	}
	if goalSpecified {
		goal, err := readGoal(*goalFile)
		if err != nil {
			return fail("invalid goal file or JSON")
		}
		if _, err := growth.VerifyGoal(*goal, *p, *workspace, *environment); err != nil {
			return fail("invalid or inconsistent goal artifacts")
		}
	}
	r, err := readReviewRecord(*record)
	if err != nil {
		return fail("invalid review record file or JSON")
	}
	result, err := growth.VerifyReview(*p, *r, *workspace, *environment)
	if err != nil {
		return fail("invalid or inconsistent review artifacts")
	}
	data, err := json.Marshal(result)
	if err == nil {
		var n int
		data = append(data, '\n')
		n, err = out.Write(data)
		if err == nil && n != len(data) {
			err = io.ErrShortWrite
		}
	}
	if err != nil {
		fmt.Fprintln(diagnostics, "output failed")
		return 1
	}
	return 0
}

// Nonblocking, no-follow open rejects symlinks and special files before reading.
func readReviewRecord(path string) (*growth.ReviewRecord, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, growth.ErrReviewVerification
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > growth.MaxReviewRecordBytes {
		return nil, growth.ErrReviewVerification
	}
	return growth.DecodeReviewRecord(f)
}
