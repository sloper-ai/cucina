// SPDX-License-Identifier: FSL-1.1-ALv2

// Package imds is a small EC2 Instance Metadata Service (IMDSv2 only) client
// for cucina-worker-agent: session token, user data (boot data), the signed
// instance identity document with its RSA-2048 PKCS#7 signature, instance tags
// (instance-metadata tags enabled by the controller, R-POOL-1/-3) and the Spot
// instance-action notice.
//
// The client never uses an HTTP proxy (proxy variables from WorkerSettings.env
// must not divert link-local metadata traffic) and never logs response bodies.
package imds

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultEndpoint is the IPv4 IMDS endpoint.
const DefaultEndpoint = "http://169.254.169.254"

// TokenTTL is the IMDSv2 session token lifetime requested (the maximum).
const TokenTTL = 6 * time.Hour

// maxBody bounds every response (user data is at most 16 KiB on EC2).
const maxBody = 64 << 10

// ErrNotFound is returned for 404 responses: no user data, instance tags not
// enabled, no Spot action pending.
var ErrNotFound = errors.New("imds: not found")

// HTTPError is a non-404 HTTP failure. Status 5xx and 429 are transient.
type HTTPError struct {
	Path   string
	Status int
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("imds: GET %s: HTTP %d", e.Path, e.Status)
}

// Temporary reports whether retrying may help.
func (e *HTTPError) Temporary() bool {
	return e.Status >= 500 || e.Status == http.StatusTooManyRequests
}

// IsTransient reports whether err is a retryable IMDS failure (network error,
// timeout, HTTP 5xx/429). ErrNotFound and HTTP 4xx are not transient.
func IsTransient(err error) bool {
	if err == nil || errors.Is(err, ErrNotFound) {
		return false
	}
	var he *HTTPError
	if errors.As(err, &he) {
		return he.Temporary()
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	var ue *url.Error
	return errors.As(err, &ue) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, context.DeadlineExceeded)
}

// Client is an IMDSv2 client. It is safe for concurrent use.
type Client struct {
	base string
	hc   *http.Client
	now  func() time.Time

	mu       sync.Mutex
	token    string
	tokenExp time.Time
}

// New returns a client for endpoint (DefaultEndpoint when empty). Requests
// time out after 5 s unless the context expires earlier.
func New(endpoint string) *Client {
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	return &Client{
		base: strings.TrimRight(endpoint, "/"),
		hc: &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				Proxy:                 nil, // never proxy link-local metadata traffic
				DialContext:           (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
				ResponseHeaderTimeout: 5 * time.Second,
				MaxIdleConns:          2,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		now: time.Now,
	}
}

func (c *Client) sessionToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && c.now().Before(c.tokenExp) {
		return c.token, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base+"/latest/api/token", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", strconv.Itoa(int(TokenTTL/time.Second)))
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("imds: token: %w", err)
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return "", &HTTPError{Path: "/latest/api/token", Status: resp.StatusCode}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return "", fmt.Errorf("imds: token: %w", err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", errors.New("imds: empty session token")
	}
	c.token = tok
	c.tokenExp = c.now().Add(TokenTTL - 5*time.Minute)
	return tok, nil
}

func (c *Client) invalidateToken() {
	c.mu.Lock()
	c.token = ""
	c.mu.Unlock()
}

// Get fetches an IMDS path (for example "/latest/meta-data/instance-id").
func (c *Client) Get(ctx context.Context, path string) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		tok, err := c.sessionToken(ctx)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("X-aws-ec2-metadata-token", tok)
		resp, err := c.hc.Do(req)
		if err != nil {
			return nil, fmt.Errorf("imds: GET %s: %w", path, err)
		}
		switch resp.StatusCode {
		case http.StatusOK:
			b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
			drain(resp)
			if err != nil {
				return nil, fmt.Errorf("imds: GET %s: %w", path, err)
			}
			return b, nil
		case http.StatusNotFound:
			drain(resp)
			return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
		case http.StatusUnauthorized:
			drain(resp)
			c.invalidateToken()
			if attempt == 0 {
				continue // token expired or IMDS restarted: fetch a new one once
			}
			return nil, &HTTPError{Path: path, Status: resp.StatusCode}
		default:
			drain(resp)
			return nil, &HTTPError{Path: path, Status: resp.StatusCode}
		}
	}
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
	_ = resp.Body.Close()
}

// UserData returns the raw user data (ErrNotFound when the instance has none).
func (c *Client) UserData(ctx context.Context) ([]byte, error) {
	return c.Get(ctx, "/latest/user-data")
}

// InstanceID returns the instance ID.
func (c *Client) InstanceID(ctx context.Context) (string, error) {
	b, err := c.Get(ctx, "/latest/meta-data/instance-id")
	return strings.TrimSpace(string(b)), err
}

// IdentityDocument is the subset of the instance identity document the agent
// uses. The raw bytes are what the controller verifies; never re-marshal them.
type IdentityDocument struct {
	AccountID        string    `json:"accountId"`
	Architecture     string    `json:"architecture"`
	AvailabilityZone string    `json:"availabilityZone"`
	ImageID          string    `json:"imageId"`
	InstanceID       string    `json:"instanceId"`
	InstanceType     string    `json:"instanceType"`
	PendingTime      time.Time `json:"pendingTime"`
	PrivateIP        string    `json:"privateIp"`
	Region           string    `json:"region"`
}

// ParseIdentityDocument parses the identity document and checks the fields
// the agent depends on.
func ParseIdentityDocument(raw []byte) (IdentityDocument, error) {
	var d IdentityDocument
	if err := json.Unmarshal(raw, &d); err != nil {
		return IdentityDocument{}, fmt.Errorf("imds: identity document: %w", err)
	}
	switch {
	case d.InstanceID == "":
		return IdentityDocument{}, errors.New("imds: identity document has no instanceId")
	case d.Region == "":
		return IdentityDocument{}, errors.New("imds: identity document has no region")
	}
	return d, nil
}

// IdentitySignaturePath is the RSA-2048 PKCS#7 signature of the identity
// document — the only form the controller accepts (EnrollWorkerRequest.signature);
// /signature is the RSA-1024 form and must not be used.
const IdentitySignaturePath = "/latest/dynamic/instance-identity/rsa2048"

// Identity returns the raw instance identity document, its parsed form and the
// base64 PKCS#7 RSA-2048 signature over it (the body of IdentitySignaturePath,
// whitespace removed).
func (c *Client) Identity(ctx context.Context) (raw []byte, doc IdentityDocument, signature string, err error) {
	raw, err = c.Get(ctx, "/latest/dynamic/instance-identity/document")
	if err != nil {
		return nil, IdentityDocument{}, "", err
	}
	if doc, err = ParseIdentityDocument(raw); err != nil {
		return nil, IdentityDocument{}, "", err
	}
	sig, err := c.Get(ctx, IdentitySignaturePath)
	if err != nil {
		return nil, IdentityDocument{}, "", err
	}
	signature = string(bytes.Join(bytes.Fields(sig), nil)) // the body is base64 wrapped at 64 columns
	if signature == "" {
		return nil, IdentityDocument{}, "", errors.New("imds: empty identity signature")
	}
	return raw, doc, signature, nil
}

// Tags returns the instance tags whose key starts with prefix ("" = all).
// ErrNotFound means instance-metadata tags are not enabled.
func (c *Client) Tags(ctx context.Context, prefix string) (map[string]string, error) {
	list, err := c.Get(ctx, "/latest/meta-data/tags/instance")
	if err != nil {
		return nil, err
	}
	tags := map[string]string{}
	for _, key := range ParseTagKeys(list) {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		v, err := c.Get(ctx, "/latest/meta-data/tags/instance/"+url.PathEscape(key))
		if err != nil {
			return nil, err
		}
		tags[key] = string(v)
	}
	return tags, nil
}

// ParseTagKeys splits the newline-separated tag key listing.
func ParseTagKeys(list []byte) []string {
	var keys []string
	for _, line := range strings.Split(string(list), "\n") {
		if k := strings.TrimSpace(line); k != "" {
			keys = append(keys, k)
		}
	}
	return keys
}

// InstanceAction is a pending Spot interruption notice.
type InstanceAction struct {
	Action string    `json:"action"` // terminate | stop | hibernate
	Time   time.Time `json:"time"`
}

// ParseInstanceAction parses /latest/meta-data/spot/instance-action.
func ParseInstanceAction(raw []byte) (InstanceAction, error) {
	var a InstanceAction
	if err := json.Unmarshal(raw, &a); err != nil {
		return InstanceAction{}, fmt.Errorf("imds: spot instance-action: %w", err)
	}
	if a.Action == "" {
		return InstanceAction{}, errors.New("imds: spot instance-action without action")
	}
	return a, nil
}

// SpotInstanceAction returns the pending Spot interruption notice, or nil when
// none is pending (also for on-demand instances).
func (c *Client) SpotInstanceAction(ctx context.Context) (*InstanceAction, error) {
	raw, err := c.Get(ctx, "/latest/meta-data/spot/instance-action")
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a, err := ParseInstanceAction(raw)
	if err != nil {
		return nil, err
	}
	return &a, nil
}
