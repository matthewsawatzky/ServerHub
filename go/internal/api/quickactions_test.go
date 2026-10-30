package api

import (
	"testing"

	"serverhub/internal/config"
)

func TestCleanQuickActions(t *testing.T) {
	got := cleanQuickActions([]config.QuickAction{
		{Label: "Restart nginx", Command: "true"},
		{Label: "Restart nginx", Command: "false"},
		{Label: "Nightly", Command: "true", Mode: "detached", Session: "nightly"},
	})
	if len(got) != 3 {
		t.Fatalf("got %d actions, want 3", len(got))
	}
	if got[0].ID != "restart-nginx" || got[1].ID != "restart-nginx-2" {
		t.Errorf("ids = %q, %q", got[0].ID, got[1].ID)
	}
	if got[2].Mode != "detached" || got[2].Session != "nightly" {
		t.Errorf("detached fields lost: %+v", got[2])
	}

	local := cleanLocalQuickActions([]config.QuickAction{
		{Label: "Nightly", Command: "true", Mode: "detached", Session: "nightly"},
	})
	if local[0].Mode != "" || local[0].Session != "" {
		t.Errorf("local action kept detached mode: %+v", local[0])
	}
}

func TestMergeQuickActionStateByID(t *testing.T) {
	old := []config.QuickAction{
		{ID: "stable", Label: "Old label", Command: "old", LastRan: 123, LastStatus: "success"},
		{ID: "other", Label: "Keep", Command: "true", LastRan: 5, LastStatus: "error"},
	}
	next := []config.QuickAction{
		{ID: "stable", Label: "New label", Command: "new"},
		{ID: "renamed", Label: "Keep", Command: "true"},
	}
	mergeQuickActionState(old, next)

	if next[0].LastRan != 123 || next[0].LastStatus != "success" {
		t.Errorf("stable ID did not carry state: %+v", next[0])
	}
	if next[1].LastRan != 5 || next[1].LastStatus != "error" {
		t.Errorf("label+command fallback did not carry state: %+v", next[1])
	}
}
