package api

import (
	"fmt"
	"sync"
	"time"
)

// maxAuditEntries bounds the in-memory log so a long session cannot grow it
// without limit (same idea as the debug collector's ring caps).
const maxAuditEntries = 2000

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
	if len(globalAudit.entries) > maxAuditEntries {
		globalAudit.entries = globalAudit.entries[len(globalAudit.entries)-maxAuditEntries:]
	}
}

func GetAuditLog() []AuditEntry {
	globalAudit.mu.RLock()
	defer globalAudit.mu.RUnlock()
	out := make([]AuditEntry, len(globalAudit.entries))
	copy(out, globalAudit.entries)
	return out
}
