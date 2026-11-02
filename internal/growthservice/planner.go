package growthservice

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// LocalPlanner connects only to an explicitly configured loopback adapter. It
// has no API key, cloud fallback, tool execution or retry. Output is untrusted.
type LocalPlanner struct {
	origin string
	client http.Client
}

func NewLocalPlanner(origin string) (*LocalPlanner, error) {
	u, e := url.Parse(origin)
	if e != nil || u == nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || u.ForceQuery {
		return nil, ErrInput
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return nil, ErrInput
	}
	return &LocalPlanner{origin: origin, client: http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (p *LocalPlanner) Plan(ctx context.Context, in PlannerInput) ([]byte, error) {
	b, e := json.Marshal(in)
	if e != nil {
		return nil, ErrInput
	}
	req, e := http.NewRequestWithContext(ctx, "POST", p.origin+"/plan", bytes.NewReader(b))
	if e != nil {
		return nil, ErrInput
	}
	req.Header.Set("Content-Type", "application/json")
	resp, e := p.client.Do(req)
	if e != nil {
		return nil, ErrInput
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, ErrInput
	}
	out, e := io.ReadAll(io.LimitReader(resp.Body, 16385))
	if e != nil || len(out) > 16384 {
		return nil, ErrInput
	}
	return out, nil
}
