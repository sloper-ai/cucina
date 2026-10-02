// SPDX-License-Identifier: FSL-1.1-ALv2

package link

import (
	"time"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
)

type queuedMetric struct {
	message *cucinav1.HostMessage
	at      time.Time
}

// MetricsSession captures the current session before a fresh scrape. Protocol
// 1.0 controllers never receive the additive snapshot message.
func (l *Link) MetricsSession() (int, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sessions, l.connected && l.welcome.GetProtocol().GetMinor() >= 1
}

// SendMetrics queues a newly scraped snapshot only for the same live session.
// These messages are replaceable, expire after 15 seconds and are NEVER replayed
// on reconnect. Durable commands/events use Send instead.
func (l *Link) SendMetrics(session int, snapshot *cucinav1.MetricsSnapshot) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.connected || session != l.sessions || l.welcome.GetProtocol().GetMinor() < 1 {
		return
	}
	key := snapshot.GetSource().String() + "/" + snapshot.GetVmName()
	if l.metrics == nil {
		l.metrics = map[string]queuedMetric{}
	}
	if _, exists := l.metrics[key]; !exists && len(l.metrics) >= 4 {
		return
	}
	l.metrics[key] = queuedMetric{message: &cucinav1.HostMessage{Message: &cucinav1.HostMessage_MetricsSnapshot{MetricsSnapshot: snapshot}}, at: l.o.Clock.Now()}
	select {
	case l.notify <- struct{}{}:
	default:
	}
}

func (l *Link) takeMetrics(session int) []*cucinav1.HostMessage {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.connected || session != l.sessions {
		return nil
	}
	var out []*cucinav1.HostMessage
	for key, queued := range l.metrics {
		delete(l.metrics, key)
		if l.o.Clock.Now().Sub(queued.at) < 15*time.Second {
			out = append(out, queued.message)
		}
	}
	return out
}
