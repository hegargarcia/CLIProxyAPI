package auth

import (
	"context"
	"strconv"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func claudeResetAuth(id string, resetAt time.Time) *Auth {
	return &Auth{
		ID:       id,
		Provider: "claude",
		Quota: QuotaState{
			ObservedAt: resetAt.Add(-time.Hour),
			Signals: map[string]string{
				"Anthropic-Ratelimit-Unified-7d-Reset": strconv.FormatInt(resetAt.Unix(), 10),
			},
		},
	}
}

func TestResetFirstSelectorPick_PrefersSoonestReset(t *testing.T) {
	t.Parallel()

	now := time.Now()
	selector := &ResetFirstSelector{}
	auths := []*Auth{
		claudeResetAuth("far", now.Add(4*time.Hour)),
		claudeResetAuth("soon", now.Add(30*time.Minute)),
		claudeResetAuth("mid", now.Add(2*time.Hour)),
	}

	got, err := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got == nil || got.ID != "soon" {
		t.Fatalf("Pick() auth = %+v, want soon", got)
	}
}

func TestOrderByQuotaReset_UnknownAndElapsedSortLast(t *testing.T) {
	t.Parallel()

	now := time.Now()
	auths := []*Auth{
		{ID: "b-no-signal", Provider: "claude"},
		claudeResetAuth("elapsed", now.Add(-time.Minute)),
		claudeResetAuth("active", now.Add(3*time.Hour)),
		{ID: "a-no-signal", Provider: "claude"},
	}

	ordered := orderByQuotaReset(auths, now)
	want := []string{"active", "a-no-signal", "b-no-signal", "elapsed"}
	for i, id := range want {
		if ordered[i].ID != id {
			t.Fatalf("ordered[%d] = %q, want %q (full order %v)", i, ordered[i].ID, id, authIDs(ordered))
		}
	}
}

func TestQuotaResetAt_CodexRelativeAndAbsolute(t *testing.T) {
	t.Parallel()

	now := time.Now()
	observed := now.Add(-10 * time.Minute)

	relative := &Auth{ID: "relative", Provider: "codex", Quota: QuotaState{
		ObservedAt: observed,
		Signals:    map[string]string{"X-Codex-Secondary-Reset-After-Seconds": "3600"},
	}}
	got, ok := quotaResetAt(relative, now)
	if !ok || !got.Equal(observed.Add(time.Hour)) {
		t.Fatalf("relative reset = %v ok=%v, want %v", got, ok, observed.Add(time.Hour))
	}

	absolute := &Auth{ID: "absolute", Provider: "codex", Quota: QuotaState{
		ObservedAt: observed,
		Signals: map[string]string{
			"X-Codex-Secondary-Reset-At":            strconv.FormatInt(now.Add(2*time.Hour).Unix(), 10),
			"X-Codex-Secondary-Reset-After-Seconds": "60",
		},
	}}
	got, ok = quotaResetAt(absolute, now)
	if !ok || !got.Equal(time.Unix(now.Add(2*time.Hour).Unix(), 0)) {
		t.Fatalf("absolute reset = %v ok=%v, want Reset-At to win over Reset-After-Seconds", got, ok)
	}

	// Unknown providers never report a reset even when signals exist.
	other := &Auth{ID: "other", Provider: "gemini", Quota: QuotaState{
		Signals: map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": strconv.FormatInt(now.Add(time.Hour).Unix(), 10)},
	}}
	if _, ok := quotaResetAt(other, now); ok {
		t.Fatal("unexpected reset for provider without reset signal mapping")
	}
}

func authIDs(auths []*Auth) []string {
	ids := make([]string, 0, len(auths))
	for _, auth := range auths {
		ids = append(ids, auth.ID)
	}
	return ids
}
