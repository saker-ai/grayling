// Package nacos provides the Nacos 3 Admin API calls used by the external
// substrate. It never exposes administrator credentials to SDK callers.
package nacos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var ErrNotFound = errors.New("nacos: resource not found")

type Config struct {
	URL      string
	Username string
	Password string
	Token    string
}

type Client struct {
	base   *url.URL
	http   *http.Client
	config Config
	mu     sync.Mutex
	token  string
	expiry time.Time
	known  map[string]bool
}

func NewClient(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("nacos: URL must be an http(s) origin")
	}
	if (cfg.Username == "") != (cfg.Password == "") || (cfg.Token != "" && cfg.Username != "") {
		return nil, fmt.Errorf("nacos: provide username and password together, or a token")
	}
	u.Path = "/"
	return &Client{base: u, config: cfg, known: make(map[string]bool), http: &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

type response struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type apiError struct {
	Code int
	Text string
}

func (e *apiError) Error() string { return fmt.Sprintf("nacos: code %d: %s", e.Code, e.Text) }

func (c *Client) authToken(ctx context.Context) (string, error) {
	if c.config.Token != "" {
		return c.config.Token, nil
	}
	if c.config.Username == "" {
		return "", nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.expiry) {
		return c.token, nil
	}
	form := url.Values{"username": {c.config.Username}, "password": {c.config.Password}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base.String()+"nacos/v3/auth/user/login", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("nacos: login: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("nacos: login returned HTTP %d", resp.StatusCode)
	}
	var login struct {
		AccessToken string `json:"accessToken"`
		TokenTTL    int64  `json:"tokenTtl"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&login); err != nil || login.AccessToken == "" {
		return "", fmt.Errorf("nacos: invalid login response: %v", err)
	}
	c.token = login.AccessToken
	c.expiry = time.Now().Add(time.Duration(max(1, login.TokenTTL-60)) * time.Second)
	return c.token, nil
}

func (c *Client) call(ctx context.Context, method, path string, values url.Values, retryAuth bool) (json.RawMessage, error) {
	token, err := c.authToken(ctx)
	if err != nil {
		return nil, err
	}
	var body io.Reader
	endpoint := c.base.String() + strings.TrimPrefix(path, "/")
	if method == http.MethodPost || method == http.MethodPut {
		body = strings.NewReader(values.Encode())
	} else if len(values) > 0 {
		endpoint += "?" + values.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("accessToken", token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("nacos: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized && retryAuth && c.config.Username != "" {
		_ = resp.Body.Close()
		c.mu.Lock()
		if c.token == token {
			c.token = ""
			c.expiry = time.Time{}
		}
		c.mu.Unlock()
		return c.call(ctx, method, path, values, false)
	}
	var result response
	if err := json.NewDecoder(io.LimitReader(resp.Body, 10<<20)).Decode(&result); err != nil {
		return nil, fmt.Errorf("nacos: %s %s: HTTP %d: decode response: %w", method, path, resp.StatusCode, err)
	}
	if resp.StatusCode == http.StatusNotFound || result.Code == 20004 || result.Code == 22001 {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || result.Code != 0 {
		return nil, &apiError{Code: result.Code, Text: result.Message}
	}
	return result.Data, nil
}

func (c *Client) request(ctx context.Context, method, path string, values url.Values) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return c.call(ctx, method, path, values, true)
}

func (c *Client) Ping() error {
	return c.PingContext(context.Background())
}

func (c *Client) PingContext(ctx context.Context) error {
	_, err := c.request(ctx, http.MethodGet, "/nacos/v3/admin/core/namespace/list", nil)
	return err
}

func (c *Client) EnsureNamespace(namespace string) error {
	return c.EnsureNamespaceContext(context.Background(), namespace)
}

func (c *Client) EnsureNamespaceContext(ctx context.Context, namespace string) error {
	if namespace == "" {
		return fmt.Errorf("nacos: empty namespace")
	}
	c.mu.Lock()
	known := c.known[namespace]
	c.mu.Unlock()
	if known {
		return nil
	}
	var exists bool
	data, err := c.request(ctx, http.MethodGet, "/nacos/v3/admin/core/namespace/check", url.Values{"namespaceId": {namespace}})
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &exists); err != nil {
		return fmt.Errorf("nacos: namespace check: %w", err)
	}
	if !exists {
		created, createErr := c.request(ctx, http.MethodPost, "/nacos/v3/admin/core/namespace", url.Values{
			"namespaceId": {namespace}, "namespaceName": {namespace}, "namespaceDesc": {"Grayling managed"},
		})
		var apiErr *apiError
		if createErr != nil && (!errors.As(createErr, &apiErr) || apiErr.Code != 22002) {
			return fmt.Errorf("nacos: create namespace %s: %w", namespace, createErr)
		}
		if createErr == nil {
			var ok bool
			if err := json.Unmarshal(created, &ok); err != nil || !ok {
				return fmt.Errorf("nacos: namespace creation was not acknowledged: %v", err)
			}
		}
	}
	c.mu.Lock()
	c.known[namespace] = true
	c.mu.Unlock()
	return nil
}

func (c *Client) Get(namespace, group, dataID string) (string, error) {
	return c.GetContext(context.Background(), namespace, group, dataID)
}

func (c *Client) GetContext(ctx context.Context, namespace, group, dataID string) (string, error) {
	data, err := c.request(ctx, http.MethodGet, "/nacos/v3/admin/cs/config", url.Values{
		"namespaceId": {namespace}, "groupName": {group}, "dataId": {dataID},
	})
	if err != nil {
		return "", err
	}
	var item struct {
		Content *string `json:"content"`
	}
	if err := json.Unmarshal(data, &item); err != nil {
		return "", fmt.Errorf("nacos: parse config: %w", err)
	}
	if item.Content == nil {
		return "", fmt.Errorf("nacos: config response has no content")
	}
	return *item.Content, nil
}

func (c *Client) Publish(namespace, group, dataID, content string) error {
	return c.PublishContext(context.Background(), namespace, group, dataID, content)
}

func (c *Client) PublishContext(ctx context.Context, namespace, group, dataID, content string) error {
	if err := c.EnsureNamespaceContext(ctx, namespace); err != nil {
		return err
	}
	values := url.Values{
		"namespaceId": {namespace}, "groupName": {group}, "dataId": {dataID},
		"content": {content}, "type": {"text"}, "appName": {"grayling"},
	}
	data, err := c.request(ctx, http.MethodPost, "/nacos/v3/admin/cs/config", values)
	if errors.Is(err, ErrNotFound) {
		c.mu.Lock()
		delete(c.known, namespace)
		c.mu.Unlock()
		if ensureErr := c.EnsureNamespaceContext(ctx, namespace); ensureErr != nil {
			return ensureErr
		}
		data, err = c.request(ctx, http.MethodPost, "/nacos/v3/admin/cs/config", values)
	}
	if err != nil {
		return err
	}
	var ok bool
	if err := json.Unmarshal(data, &ok); err != nil || !ok {
		return fmt.Errorf("nacos: publish was not acknowledged: %v", err)
	}
	return nil
}

func (c *Client) Delete(namespace, group, dataID string) error {
	return c.DeleteContext(context.Background(), namespace, group, dataID)
}

func (c *Client) DeleteContext(ctx context.Context, namespace, group, dataID string) error {
	data, err := c.request(ctx, http.MethodDelete, "/nacos/v3/admin/cs/config", url.Values{
		"namespaceId": {namespace}, "groupName": {group}, "dataId": {dataID},
	})
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var ok bool
	if err := json.Unmarshal(data, &ok); err != nil || !ok {
		return fmt.Errorf("nacos: delete was not acknowledged: %v", err)
	}
	return nil
}
