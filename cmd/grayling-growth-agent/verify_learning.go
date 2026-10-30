package main

import (
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/saker-ai/grayling/internal/growth"
)

func runVerifyLearning(args []string, out, diagnostics io.Writer) int {
	fail := func(message string) int { fmt.Fprintln(diagnostics, message); return 2 }
	fs := flag.NewFlagSet("verify-learning", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	proposal := fs.String("proposal", "", "original proposal file")
	record := fs.String("record", "", "exported learning record file")
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
	r, _, err := readLearningRecord(*record, growth.MaxLearningRecordBytes)
	if err != nil {
		return fail("invalid learning record file or JSON")
	}
	result, err := growth.VerifyLearning(*p, *r, *workspace, *environment)
	if err != nil {
		return fail("invalid or inconsistent learning artifacts")
	}
	data, err := json.Marshal(result)
	if err == nil {
		_, err = out.Write(append(data, '\n'))
	}
	if err != nil {
		fmt.Fprintln(diagnostics, "output failed")
		return 1
	}
	return 0
}
