// SPDX-License-Identifier: FSL-1.1-ALv2

// Package imdsfake is a stateful in-process fake of the EC2 instance metadata
// service (IMDSv2 only) for cucina-worker-agent tests: session tokens, user
// data, the identity document + its RSA-2048 PKCS#7 signature (/rsa2048 only;
// the RSA-1024 /signature form is absent on purpose), instance tags, the Spot
// instance-action notice and injected failures. It serves plain HTTP on
// localhost through net/http/httptest.
package imdsfake

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
)

// Server is the fake IMDS. Zero values mean "absent" (404).
type Server struct {
	*httptest.Server

	mu          sync.Mutex
	tokens      map[string]bool
	tokenPuts   int
	userData    []byte
	document    []byte
	signature   string
	instanceID  string
	tags        map[string]string // nil = instance-metadata tags disabled
	spotAction  []byte
	failures    map[string][]int // path -> queued HTTP status codes
	getRequests map[string]int
}

// New starts a fake IMDS; it is closed when the test ends.
func New(t interface{ Cleanup(func()) }) *Server {
	s := &Server{tokens: map[string]bool{}, failures: map[string][]int{}, getRequests: map[string]int{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// SetUserData sets the raw user data.
func (s *Server) SetUserData(b []byte) { s.mu.Lock(); s.userData = b; s.mu.Unlock() }

// SetIdentity sets the identity document, the body of
// /latest/dynamic/instance-identity/rsa2048 and the instance ID.
func (s *Server) SetIdentity(document []byte, signature, instanceID string) {
	s.mu.Lock()
	s.document, s.signature, s.instanceID = document, signature, instanceID
	s.mu.Unlock()
}

// SetTags enables instance-metadata tags with the given tags (nil disables).
func (s *Server) SetTags(tags map[string]string) { s.mu.Lock(); s.tags = tags; s.mu.Unlock() }

// SetSpotAction sets the raw Spot instance-action document (nil = none).
func (s *Server) SetSpotAction(b []byte) { s.mu.Lock(); s.spotAction = b; s.mu.Unlock() }

// FailNext makes the next len(statuses) requests for path fail with the given
// HTTP status codes, in order. Path "/latest/api/token" fails token requests.
func (s *Server) FailNext(path string, statuses ...int) {
	s.mu.Lock()
	s.failures[path] = append(s.failures[path], statuses...)
	s.mu.Unlock()
}

// ExpireTokens forgets every issued token (the next GET gets 401).
func (s *Server) ExpireTokens() { s.mu.Lock(); s.tokens = map[string]bool{}; s.mu.Unlock() }

// TokenRequests returns how many tokens were issued.
func (s *Server) TokenRequests() int { s.mu.Lock(); defer s.mu.Unlock(); return s.tokenPuts }

// GetRequests returns how many authorised GETs reached path.
func (s *Server) GetRequests(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getRequests[path]
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if q := s.failures[r.URL.Path]; len(q) > 0 {
		s.failures[r.URL.Path] = q[1:]
		http.Error(w, "injected failure", q[0])
		return
	}
	if r.URL.Path == "/latest/api/token" {
		if r.Method != http.MethodPut || r.Header.Get("X-aws-ec2-metadata-token-ttl-seconds") == "" {
			http.Error(w, "bad token request", http.StatusBadRequest)
			return
		}
		s.tokenPuts++
		tok := fmt.Sprintf("token-%d", s.tokenPuts)
		s.tokens[tok] = true
		_, _ = w.Write([]byte(tok))
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.tokens[r.Header.Get("X-aws-ec2-metadata-token")] {
		http.Error(w, "unauthorized", http.StatusUnauthorized) // IMDSv1 is disabled (R-POOL-1)
		return
	}
	s.getRequests[r.URL.Path]++
	body, ok := s.lookup(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, _ = w.Write(body)
}

func (s *Server) lookup(path string) ([]byte, bool) {
	switch path {
	case "/latest/user-data":
		return s.userData, s.userData != nil
	case "/latest/dynamic/instance-identity/document":
		return s.document, s.document != nil
	case "/latest/dynamic/instance-identity/rsa2048":
		return []byte(s.signature), s.signature != ""
	case "/latest/meta-data/instance-id":
		return []byte(s.instanceID), s.instanceID != ""
	case "/latest/meta-data/spot/instance-action":
		return s.spotAction, s.spotAction != nil
	case "/latest/meta-data/tags/instance":
		if s.tags == nil {
			return nil, false
		}
		keys := make([]string, 0, len(s.tags))
		for k := range s.tags {
			keys = append(keys, k)
		}
		return []byte(strings.Join(keys, "\n")), true
	}
	if key, ok := strings.CutPrefix(path, "/latest/meta-data/tags/instance/"); ok && s.tags != nil {
		key, err := url.PathUnescape(key)
		if err != nil {
			return nil, false
		}
		v, ok := s.tags[key]
		return []byte(v), ok
	}
	return nil, false
}
