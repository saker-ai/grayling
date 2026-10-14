package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/saker-ai/grayling/internal/configstore"
	"github.com/saker-ai/grayling/internal/platform"
	"github.com/saker-ai/grayling/internal/recovery"
)

// runHMACRotation is an offline operation. The caller never starts listeners
// or background writers, and must arrange an external stop-write barrier.
func runHMACRotation(ctx context.Context, store *platform.Store, dsn, authSecret, redisAddr,
	snapshotHash string, cfg configstore.Store, backupPoint, keyFile, operatorFile, approverFile string) error {
	pointBytes, err := verifyRotationBackup(ctx, store, dsn, authSecret, redisAddr, snapshotHash, cfg, backupPoint)
	if err != nil {
		return err
	}
	key, err := readOwnerOnly(keyFile)
	if err != nil {
		return fmt.Errorf("rotation key: %w", err)
	}
	if len(key) != 64 {
		return fmt.Errorf("rotation key must contain 64 hex characters")
	}
	if _, err := hex.DecodeString(key); err != nil {
		return fmt.Errorf("rotation key must contain 64 hex characters")
	}
	operator, err := readRotationOwner(store, operatorFile)
	if err != nil {
		return fmt.Errorf("operator: %w", err)
	}
	approver, err := readRotationOwner(store, approverFile)
	if err != nil {
		return fmt.Errorf("approver: %w", err)
	}
	if operator.ID == approver.ID {
		return fmt.Errorf("operator and approver must have distinct owner tokens")
	}
	hash := sha256.Sum256(pointBytes)
	return store.RotateHMACKeyAudited(key, operator.ID, approver.ID, hex.EncodeToString(hash[:]))
}

func verifyRotationBackup(ctx context.Context, store *platform.Store, dsn, authSecret, redisAddr,
	snapshotHash string, cfg configstore.Store, backupPoint string) ([]byte, error) {
	if redisAddr == "" || authSecret == "" {
		return nil, fmt.Errorf("external Redis and escrowed auth secret are required")
	}
	pointBytes, err := os.ReadFile(backupPoint)
	if err != nil {
		return nil, fmt.Errorf("read backup checkpoint: %w", err)
	}
	point, err := recovery.Capture(ctx, dsn, authSecret, store, cfg)
	if err != nil {
		return nil, err
	}
	point.GonacosSHA256 = snapshotHash
	if err := recovery.Verify(backupPoint, point); err != nil {
		return nil, fmt.Errorf("backup checkpoint preflight: %w", err)
	}
	return pointBytes, nil
}

func provisionRotationApprover(ctx context.Context, store *platform.Store, dsn, authSecret, redisAddr,
	snapshotHash string, cfg configstore.Store, backupPoint, operatorFile, outputFile string) error {
	pointBytes, err := verifyRotationBackup(ctx, store, dsn, authSecret, redisAddr, snapshotHash, cfg, backupPoint)
	if err != nil {
		return err
	}
	operator, err := readRotationOwner(store, operatorFile)
	if err != nil {
		return fmt.Errorf("operator: %w", err)
	}
	file, err := os.OpenFile(outputFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create approver token file: %w", err)
	}
	defer file.Close()
	hash := sha256.Sum256(pointBytes)
	created, err := store.ProvisionRotationApprover(operator.ID, hex.EncodeToString(hash[:]))
	if err != nil {
		_ = os.Remove(outputFile)
		return err
	}
	if _, err := file.WriteString(created.Secret + "\n"); err != nil {
		_ = store.RevokeToken(created.Token.ID)
		_ = os.Remove(outputFile)
		return fmt.Errorf("write approver token file: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = store.RevokeToken(created.Token.ID)
		_ = os.Remove(outputFile)
		return fmt.Errorf("sync approver token file: %w", err)
	}
	return nil
}

func readOwnerOnly(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > 4096 {
		return "", fmt.Errorf("input must be a regular owner-only file (mode 0600)")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return "", fmt.Errorf("input file changed during open")
	}
	b, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return "", err
	}
	if len(b) > 4096 {
		return "", fmt.Errorf("input file too large")
	}
	value := strings.TrimSpace(string(b))
	if value == "" || strings.ContainsAny(value, " \t\r\n") {
		return "", fmt.Errorf("input must contain one non-empty value")
	}
	return value, nil
}

func readRotationOwner(store *platform.Store, path string) (*platform.Token, error) {
	secret, err := readOwnerOnly(path)
	if err != nil {
		return nil, err
	}
	token, err := store.ResolveToken(secret)
	if err != nil {
		return nil, fmt.Errorf("owner authorization failed: %w", err)
	}
	if token.Kind != platform.TokenAdmin || token.Role != platform.RoleOwner || token.WorkspaceID != "" || token.Revoked {
		return nil, fmt.Errorf("global owner token required")
	}
	return token, nil
}
