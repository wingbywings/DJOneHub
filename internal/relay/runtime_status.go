package relay

import (
	"context"
	"sort"
	"time"

	"github.com/iniwex5/vohive/internal/relaystatus"
)

func (s *Server) presence() relaystatus.Observation {
	s.signals.mu.Lock()
	defer s.signals.mu.Unlock()
	ids := make([]string, 0, len(s.signals.connections))
	for id, c := range s.signals.connections {
		if c.ready && c.ctx.Err() == nil {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	now := time.Now().UTC()
	return relaystatus.Observe(relaystatus.Snapshot{Version: 1, ObservedAt: now, Running: !s.signals.closed, OnlineIdentities: ids, OnlineCount: len(ids)}, now)
}

func (s *Server) RuntimeSnapshot(ctx context.Context) relaystatus.Snapshot {
	snapshot := *s.presence().Snapshot
	snapshot.Build, snapshot.StartedAt = s.config.build, s.started
	check, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	snapshot.IdentityHealthy = s.signals.store.Health(check) == nil
	snapshot.TURN = s.turnActivity.Snapshot(s.config.turn != nil, "")
	environment := ""
	if s.config.push != nil {
		environment = s.config.push.Environment()
	}
	snapshot.APNs = s.signals.pushActivity.Snapshot(s.config.push != nil, environment)
	return snapshot
}
