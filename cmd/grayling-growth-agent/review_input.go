package main

import (
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"syscall"

	"github.com/saker-ai/grayling/internal/growth"
)

const maxReviewInputBytes = 16 << 10

// Review input is operator-owned, separate from proposal and evidence. JSON v2
// rejects duplicates (including escaped aliases), trailing values and invalid
// UTF-8; the struct allowlist excludes authorization and embedded evidence.
func readReviewInputs(path string) (*growth.ReviewInputs, error) {
	invalid := errors.New("invalid review input")
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, invalid
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxReviewInputBytes {
		return nil, invalid
	}
	data, err := io.ReadAll(io.LimitReader(f, maxReviewInputBytes+1))
	if err != nil || len(data) > maxReviewInputBytes {
		return nil, invalid
	}
	var in *growth.ReviewInputs
	if json.Unmarshal(data, &in, json.RejectUnknownMembers(true)) != nil || in == nil {
		return nil, invalid
	}
	return in, nil
}
