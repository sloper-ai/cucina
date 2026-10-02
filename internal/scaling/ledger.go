// SPDX-License-Identifier: FSL-1.1-ALv2

package scaling

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/ports"
)

// Launch tokens are "cuc-<hash(cluster,pool)>-<epoch>-<seq>" (≤ 64 ASCII
// characters, the EC2 ClientToken limit). The hash keeps tokens of different
// installations and pools apart; the epoch keeps a recreated ledger apart from
// every older one; seq is the ledger position.

const tokenPrefixTag = "cuc-"

// TokenPrefix returns the token prefix of a pool in a cluster.
func TokenPrefix(cluster string, pool domain.PoolName) string {
	h := sha256.Sum256([]byte(cluster + "\x00" + string(pool)))
	return tokenPrefixTag + hex.EncodeToString(h[:5])
}

// MakeToken builds a launch token.
func MakeToken(prefix, epoch string, seq uint64) string {
	return prefix + "-" + epoch + "-" + strconv.FormatUint(seq, 10)
}

// ParseToken splits a launch token; ok is false for anything not produced by MakeToken.
func ParseToken(tok string) (prefix, epoch string, seq uint64, ok bool) {
	if !strings.HasPrefix(tok, tokenPrefixTag) {
		return "", "", 0, false
	}
	i := strings.LastIndexByte(tok, '-')
	if i <= len(tokenPrefixTag) {
		return "", "", 0, false
	}
	seq, err := strconv.ParseUint(tok[i+1:], 10, 64)
	if err != nil {
		return "", "", 0, false
	}
	rest := tok[:i]
	j := strings.LastIndexByte(rest, '-')
	if j <= len(tokenPrefixTag) || j == len(rest)-1 {
		return "", "", 0, false
	}
	return rest[:j], rest[j+1:], seq, true
}

// ensureEpoch creates the ledger epoch on first use.
func (s *PoolState) ensureEpoch(rnd ports.Rand, now time.Time) {
	if s.ledger.Epoch == "" {
		s.ledger = Ledger{Epoch: rnd.Token(4), At: now}
	}
}

// restoreFromObservation runs once, on the first successful provider
// observation of this PoolState: seqs of [Committed, Next) that no observed
// instance carries may belong to launches still in flight from a previous
// leader, so they are reused first (EC2 deduplicates them by token) and count
// as pending capacity until ConsistencyGrace after the ledger's At.
func (s *PoolState) restoreFromObservation(prefix string, instances []ports.Instance, now time.Time, grace time.Duration) {
	observed := map[uint64]bool{}
	var maxSeq uint64
	var any bool
	for _, in := range instances {
		p, e, seq, ok := ParseToken(in.Tags[domain.TagLaunchToken])
		if !ok || p != prefix || e != s.ledger.Epoch {
			continue
		}
		observed[seq] = true
		if !any || seq > maxSeq {
			maxSeq, any = seq, true
		}
	}
	if any && maxSeq+1 > s.ledger.Next {
		// The executor launched beyond the persisted ledger (write-ahead
		// skipped); never reuse a seq that is already taken.
		s.ledger.Next = maxSeq + 1
	}
	expiry := s.ledger.At.Add(grace)
	if now.Before(expiry) {
		for seq := s.ledger.Committed; seq < s.ledger.Next; seq++ {
			if !observed[seq] {
				s.reuse = append(s.reuse, seq)
				s.ghosts[seq] = expiry
			}
		}
	}
	s.commit()
}

// allocToken returns the next token: a reusable seq first, then a fresh one.
// Every launch attempt refreshes the ledger's At, which bounds how long
// unlisted seqs count as in flight after a restart (persisted write-ahead).
func (s *PoolState) allocToken(prefix string, now time.Time) (string, uint64) {
	var seq uint64
	if len(s.reuse) > 0 {
		seq = s.reuse[0]
		s.reuse = s.reuse[1:]
		delete(s.ghosts, seq)
	} else {
		seq = s.ledger.Next
		s.ledger.Next++
	}
	s.ledger.At = now
	return MakeToken(prefix, s.ledger.Epoch, seq), seq
}

// release puts a seq that was never sent to the provider back for reuse.
func (s *PoolState) release(seq uint64) {
	if !slices.Contains(s.reuse, seq) {
		s.reuse = append(s.reuse, seq)
		slices.Sort(s.reuse)
	}
}

// commit advances Committed to the lowest seq whose outcome is unknown. It
// never runs before the first provider observation: until restoreFromObservation
// has seen the fleet, the seqs in [Committed, Next) may belong to launches of a
// previous leader that are still in flight.
func (s *PoolState) commit() {
	if !s.providerSeen {
		return
	}
	low := s.ledger.Next
	for _, in := range s.intents {
		low = min(low, in.seq)
	}
	for _, seq := range s.reuse {
		low = min(low, seq)
	}
	for seq := range s.ghosts {
		low = min(low, seq)
	}
	for seq := range s.unseen {
		low = min(low, seq)
	}
	if low > s.ledger.Committed {
		s.ledger.Committed = low
	}
}

// expireGhosts forgets in-flight seqs whose grace passed without a sighting.
func (s *PoolState) expireGhosts(now time.Time) {
	for seq, until := range s.unseen {
		if !now.Before(until) {
			delete(s.unseen, seq)
		}
	}
	for seq, until := range s.ghosts {
		if !now.Before(until) {
			delete(s.ghosts, seq)
			s.reuse = slices.DeleteFunc(s.reuse, func(x uint64) bool { return x == seq })
		}
	}
}

// ledgerChanged reports (and records) whether the ledger differs from the
// value last handed out.
func (s *PoolState) ledgerChanged() bool {
	if s.ledger == s.lastLedger {
		return false
	}
	s.lastLedger = s.ledger
	return true
}
