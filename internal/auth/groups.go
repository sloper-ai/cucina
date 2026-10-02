// SPDX-License-Identifier: FSL-1.1-ALv2

package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/maypok86/otter/v2"
	"golang.org/x/oauth2/jwt"

	"github.com/sloper-ai/cucina/internal/ports"
)

// GroupBackend lists the groups an identity is a direct member of.
type GroupBackend interface {
	DirectGroups(ctx context.Context, memberKey string) ([]string, error)
}

// CachedGroups implements GroupResolver for provider "cloud-identity": it keys the
// lookup on the verified e-mail of a Google identity and caches results for the
// policy's cacheTTL (clamped to 1–10 min, default 10 min; R-AUTH-5 recommends 5–10).
// Errors are never cached, and a failed lookup fails the exchange (fail closed).
type CachedGroups struct {
	Backend GroupBackend
	cache   *otter.Cache[string, groupEntry]
}

type groupEntry struct {
	groups []string
	ttl    time.Duration
}

type otterClock struct{ c ports.Clock }

func (o otterClock) NowNano() int64                      { return o.c.Now().UnixNano() }
func (o otterClock) Tick(time.Duration) <-chan time.Time { return nil } // lazy expiry only

// NewCachedGroups wraps backend with a bounded TTL cache driven by clock.
func NewCachedGroups(backend GroupBackend, clock ports.Clock, maxEntries int) *CachedGroups {
	if maxEntries <= 0 {
		maxEntries = 10_000
	}
	c := otter.Must(&otter.Options[string, groupEntry]{
		MaximumSize: maxEntries,
		ExpiryCalculator: otter.ExpiryWritingFunc(func(e otter.Entry[string, groupEntry]) time.Duration {
			return e.Value.ttl
		}),
		Clock: otterClock{clock},
	})
	return &CachedGroups{Backend: backend, cache: c}
}

// Close stops the cache's background goroutine.
func (c *CachedGroups) Close() { c.cache.StopAllGoroutines() }

// Groups implements GroupResolver.
func (c *CachedGroups) Groups(ctx context.Context, provider string, claims ports.Claims, ttl time.Duration) ([]string, error) {
	if provider != "cloud-identity" {
		return nil, fmt.Errorf("group provider %q not supported", provider)
	}
	email, _ := claims["email"].(string)
	verified := claims["email_verified"] == true || claims["email_verified"] == "true"
	if email == "" || !verified {
		return nil, errors.New("group lookup needs a verified email claim")
	}
	ttl = min(max(ttl, time.Minute), 10*time.Minute)
	key := provider + "\x00" + strings.ToLower(email)
	if e, ok := c.cache.GetIfPresent(key); ok {
		return slices.Clone(e.groups), nil
	}
	groups, err := c.Backend.DirectGroups(ctx, email)
	if err != nil {
		return nil, err
	}
	slices.Sort(groups)
	groups = slices.Compact(groups)
	c.cache.Set(key, groupEntry{groups: groups, ttl: ttl})
	return slices.Clone(groups), nil
}

// CloudIdentityScope is the only scope the STS's Google service account needs.
const CloudIdentityScope = "https://www.googleapis.com/auth/cloud-identity.groups.readonly"

// CloudIdentity calls groups.memberships.searchDirectGroups of the Cloud Identity API
// with the STS's own service account. It is not exercised by tests (real Google calls
// are out of scope); tests use a fake GroupBackend.
type CloudIdentity struct {
	Client   *http.Client
	Endpoint string // default https://cloudidentity.googleapis.com
}

// NewCloudIdentity builds the client from a Google service-account JSON key
// (config.Auth.GroupLookup.ServiceAccountSecret).
func NewCloudIdentity(ctx context.Context, keyJSON []byte) (*CloudIdentity, error) {
	var k struct {
		Type         string `json:"type"`
		ClientEmail  string `json:"client_email"`
		PrivateKey   string `json:"private_key"`
		PrivateKeyID string `json:"private_key_id"`
		TokenURI     string `json:"token_uri"`
	}
	if err := json.Unmarshal(keyJSON, &k); err != nil {
		return nil, fmt.Errorf("parsing service account key: %w", err)
	}
	if k.Type != "service_account" || k.ClientEmail == "" || k.PrivateKey == "" {
		return nil, errors.New("not a Google service account key")
	}
	if k.TokenURI == "" {
		k.TokenURI = "https://oauth2.googleapis.com/token"
	}
	cfg := &jwt.Config{
		Email: k.ClientEmail, PrivateKey: []byte(k.PrivateKey), PrivateKeyID: k.PrivateKeyID,
		Scopes: []string{CloudIdentityScope}, TokenURL: k.TokenURI,
	}
	cl := cfg.Client(ctx)
	cl.Timeout = 10 * time.Second
	return &CloudIdentity{Client: cl}, nil
}

// DirectGroups implements GroupBackend; it returns group e-mail addresses (groupKey.id).
func (c *CloudIdentity) DirectGroups(ctx context.Context, memberKey string) ([]string, error) {
	endpoint := c.Endpoint
	if endpoint == "" {
		endpoint = "https://cloudidentity.googleapis.com"
	}
	if strings.ContainsAny(memberKey, "'\\") {
		return nil, errors.New("invalid member key")
	}
	var groups []string
	page := ""
	for i := 0; i < 50; i++ {
		q := url.Values{"query": {"member_key_id == '" + memberKey + "'"}, "pageSize": {"500"}}
		if page != "" {
			q.Set("pageToken", page)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/v1/groups/-/memberships:searchDirectGroups?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		resp, err := c.Client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("cloud identity: %w", err)
		}
		var body struct {
			Memberships []struct {
				GroupKey struct {
					ID string `json:"id"`
				} `json:"groupKey"`
			} `json:"memberships"`
			NextPageToken string `json:"nextPageToken"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("cloud identity: HTTP %d", resp.StatusCode)
		}
		if err != nil {
			return nil, fmt.Errorf("cloud identity: %w", err)
		}
		for _, m := range body.Memberships {
			if m.GroupKey.ID != "" {
				groups = append(groups, m.GroupKey.ID)
			}
		}
		if page = body.NextPageToken; page == "" {
			return groups, nil
		}
	}
	return nil, errors.New("cloud identity: too many result pages")
}
