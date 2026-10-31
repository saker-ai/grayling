package main

import (
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"syscall"

	"github.com/saker-ai/grayling/internal/growth"
)

const maxSettingsBytes = 16 << 10

// Settings are an operator-owned input, separate from the proposal and evidence.
// Decode only the compiler's input allowlist; never accept authorization or an
// evidence envelope here. Semantic validation remains in CompileCreateRequest.
func readSettings(path string) (*growth.PreparationInputs, error) {
	invalid := errors.New("invalid preparation settings")
	// O_NOFOLLOW rejects final-component symlinks, not symlinked ancestors.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, invalid
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxSettingsBytes {
		return nil, invalid
	}
	data, err := io.ReadAll(io.LimitReader(f, maxSettingsBytes+1))
	if err != nil || len(data) > maxSettingsBytes {
		return nil, invalid
	}
	var settings *growth.PreparationInputs
	if json.Unmarshal(data, &settings, json.RejectUnknownMembers(true)) != nil || settings == nil {
		return nil, invalid
	}
	return settings, nil
}
