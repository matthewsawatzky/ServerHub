package api

import (
	"fmt"
	"sync"
	"time"
)

type AuditEntry struct {
	Time     string `json:"time"`
	ServerID string `json:"server_id"`
	Action   string `json:"action"`
}

type AuditLog struct {
	mu      sync.RWMutex
	entries []AuditEntry
}

var globalAudit = &AuditLog{}

func LogAction(serverID, action string) {
	globalAudit.mu.Lock()
	defer globalAudit.mu.Unlock()
	globalAudit.entries = append(globalAudit.entries, AuditEntry{
		Time:     fmt.Sprintf("%s", time.Now().Format("2006-01-02 15:04:05")),
		ServerID: serverID,
		Action:   action,
	})
}

func GetAuditLog() []AuditEntry {
	globalAudit.mu.RLock()
	defer globalAudit.mu.RUnlock()
	out := make([]AuditEntry, len(globalAudit.entries))
	copy(out, globalAudit.entries)
	return out
}
