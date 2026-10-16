package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"google.golang.org/protobuf/proto"
)

// Download retrieves a serialized SignedResource from the authenticated origin.
// It validates the reference and wire digest, but callers must still use Verifier
// and persist the accepted resource and watermark atomically before reporting it.
// Arbitrary download origins and redirects are deliberately unsupported.
func (c *Client) Download(ctx context.Context, ref *agentv1.ResourceRef) (*agentv1.SignedResource, error) {
	if ref == nil {
		return nil, ErrResource
	}
	ref = proto.Clone(ref).(*agentv1.ResourceRef)
	scope := c.welcome.GetWelcome().Scope
	if !proto.Equal(ref.Scope, scope) {
		return nil, ErrScope
	}
	limit := min(uint64(MaxResourceBytes), c.welcome.GetWelcome().Limits.MaxResourceBytes)
	if ref.SizeBytes == 0 || ref.SizeBytes > limit || ref.Revision == 0 || ref.Key == "" || ref.Kind < 1 || ref.Kind > 4 {
		return nil, ErrResource
	}
	if ref.ExpiresUnixMs <= time.Now().UnixMilli() {
		return nil, ErrExpired
	}
	origin, _ := url.Parse(c.opts.URL)
	target, err := url.Parse(ref.HttpsUrl)
	scheme := "https"
	if origin.Scheme == "ws" {
		scheme = "http"
	}
	if err != nil || target.Scheme != scheme || !strings.EqualFold(target.Host, origin.Host) || target.User != nil || target.Fragment != "" || target.RawQuery != "" {
		return nil, ErrResource
	}
	digest, err := hex.DecodeString(ref.Sha256)
	if err != nil || len(digest) != sha256.Size {
		return nil, ErrResource
	}
	ctx, cancel := context.WithTimeout(ctx, c.opts.RequestTimeout)
	defer cancel()
	ctx, cancelExpiry := context.WithDeadline(ctx, time.UnixMilli(ref.ExpiresUnixMs))
	defer cancelExpiry()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, ErrResource
	}
	req.Header.Set("Authorization", "Bearer "+c.opts.Token)
	req.Header.Set("Accept", "application/x-protobuf")
	resp, err := c.opts.HTTPClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("agent: resource download failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &HandshakeError{StatusCode: resp.StatusCode}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, int64(ref.SizeBytes)+1))
	if err != nil {
		return nil, errors.New("agent: resource read failed")
	}
	if uint64(len(raw)) != ref.SizeBytes {
		return nil, ErrResource
	}
	actual := sha256.Sum256(raw)
	if !strings.EqualFold(hex.EncodeToString(actual[:]), ref.Sha256) {
		return nil, ErrResource
	}
	resource := new(agentv1.SignedResource)
	if err := (proto.UnmarshalOptions{RecursionLimit: 32}).Unmarshal(raw, resource); err != nil {
		return nil, ErrResource
	}
	return resource, nil
}
