package mcpgw

import (
	"testing"
	"time"
)

func TestUserSessionsAreBounded(t *testing.T) {
	u := &upstream{maxUsers: 2, users: map[string]*userSession{}}
	now := time.Now()
	u.users["old"] = &userSession{lastUsed: now.Add(-20 * time.Minute)}
	u.users["recent"] = &userSession{lastUsed: now.Add(-time.Minute)}
	u.mu.Lock()
	u.evictLocked()
	u.mu.Unlock()
	if _, ok := u.users["old"]; ok || len(u.users) != 1 {
		t.Fatalf("eviction kept %v", u.users)
	}
	u.users["idle"] = &userSession{lastUsed: now.Add(-11 * time.Minute)}
	u.reap(userSessionIdle)
	if _, ok := u.users["idle"]; ok || u.users["recent"] == nil {
		t.Fatalf("reaping left %v", u.users)
	}
}
