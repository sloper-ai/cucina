// SPDX-License-Identifier: FSL-1.1-ALv2

package workeragent

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/ports"
)

// stateSchema is the agent.state.json schema version.
const stateSchema = 1

// State is agent.state.json: what bootstrap established for this boot. It
// holds no secrets. supervise reads it for the dead-man limits, endpoints and
// certificate expiry; a second bootstrap of the same boot uses it to stay
// idempotent.
type State struct {
	Schema       int             `json:"schema"`
	AgentVersion string          `json:"agentVersion"`
	BootID       string          `json:"bootId,omitempty"`
	BootTime     time.Time       `json:"bootTime"`
	InstanceType string          `json:"instanceType,omitempty"`
	Cluster      string          `json:"cluster,omitempty"`
	Pool         string          `json:"pool"`
	Node         string          `json:"node"`
	Generation   string          `json:"generation"`
	EnrolledAt   time.Time       `json:"enrolledAt"`
	CertNotAfter time.Time       `json:"certNotAfter"`
	Settings     json.RawMessage `json:"settings"`
	Machine      Machine         `json:"machine"`
	L1Placement  string          `json:"l1Placement"`
}

// WorkerSettings decodes the stored settings.
func (s *State) WorkerSettings() (*cucinav1.WorkerSettings, error) {
	if len(s.Settings) == 0 {
		return nil, errors.New("state has no worker settings")
	}
	return DecodeSettings(s.Settings)
}

// sameBoot reports whether the state was written during the current boot.
func (s *State) sameBoot(bootID string, bootTime time.Time) bool {
	if s.BootID != "" && bootID != "" {
		return s.BootID == bootID
	}
	d := s.BootTime.Sub(bootTime)
	return d < 30*time.Second && d > -30*time.Second
}

func encodeSettings(ws *cucinav1.WorkerSettings) (json.RawMessage, error) {
	return protojson.MarshalOptions{UseProtoNames: false}.Marshal(ws)
}

// LoadState reads agent.state.json.
func LoadState(fs ports.FS, path string) (*State, error) {
	b, err := fs.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if s.Schema != stateSchema {
		return nil, fmt.Errorf("%s: schema %d is not supported (want %d)", path, s.Schema, stateSchema)
	}
	return &s, nil
}

// SaveState writes agent.state.json atomically.
func SaveState(fs ports.FS, path string, s *State) error {
	s.Schema = stateSchema
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return fs.WriteFileAtomic(path, append(b, '\n'), 0o644)
}

// The dead-man timestamp files (/run/cucina/{last-activity,last-contact};
// Windows C:\ProgramData\cucina\run\) hold Unix seconds and a newline. The
// images' shell/PowerShell dead-man timers read either the content or the
// file's modification time (written at the same moment).

func writeTimestamp(fs ports.FS, path string, t time.Time) error {
	return fs.WriteFileAtomic(path, []byte(strconv.FormatInt(t.Unix(), 10)+"\n"), 0o644)
}

func readTimestamp(fs ports.FS, path string) (time.Time, error) {
	b, err := fs.ReadFile(path)
	if err != nil {
		return time.Time{}, err
	}
	sec, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s: %w", path, err)
	}
	return time.Unix(sec, 0), nil
}
