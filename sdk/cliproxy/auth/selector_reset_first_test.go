package auth

import (
	"context"
	"reflect"
	"strconv"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func resetFirstAuth(id, provider string, fiveHourReset, weeklyReset time.Time) *Auth {
	fiveHourHeader := "Anthropic-Ratelimit-Unified-5h-Reset"
	weeklyHeader := "Anthropic-Ratelimit-Unified-7d-Reset"
	if provider == "codex" {
		fiveHourHeader = "X-Codex-Primary-Reset-At"
		weeklyHeader = "X-Codex-Secondary-Reset-At"
	}
	signals := make(map[string]string)
	for header, resetAt := range map[string]time.Time{fiveHourHeader: fiveHourReset, weeklyHeader: weeklyReset} {
		if !resetAt.IsZero() {
			signals[header] = strconv.FormatInt(resetAt.Unix(), 10)
		}
	}
	return &Auth{ID: id, Provider: provider, Quota: QuotaState{Signals: signals}}
}

func TestResetFirstSelectorPick_PrefersFiveHourResetBeforeWeekly(t *testing.T) {
	t.Parallel()

	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			now := time.Now().Truncate(time.Second)
			auths := []*Auth{
				resetFirstAuth("weekly-soon", provider, now.Add(2*time.Hour), now.Add(24*time.Hour)),
				resetFirstAuth("five-hour-soon", provider, now.Add(30*time.Minute), now.Add(5*24*time.Hour)),
			}
			got, err := (&ResetFirstSelector{}).Pick(context.Background(), provider, "", cliproxyexecutor.Options{}, auths)
			if err != nil {
				t.Fatalf("Pick() error = %v", err)
			}
			if got == nil || got.ID != "five-hour-soon" {
				t.Fatalf("Pick() = %+v, want five-hour-soon", got)
			}
		})
	}
}

func TestOrderByQuotaReset_WindowFallbacks(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 20, 2, 0, 0, 0, time.UTC)
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			for _, tc := range []struct {
				name  string
				auths []*Auth
				want  []string
			}{
				{
					name: "weekly breaks equal five-hour resets",
					auths: []*Auth{
						resetFirstAuth("a", provider, now.Add(time.Hour), now.Add(5*24*time.Hour)),
						resetFirstAuth("b", provider, now.Add(time.Hour), now.Add(24*time.Hour)),
					},
					want: []string{"b", "a"},
				},
				{
					name: "weekly decides when five-hour resets are unknown or elapsed",
					auths: []*Auth{
						resetFirstAuth("a", provider, time.Time{}, now.Add(5*24*time.Hour)),
						resetFirstAuth("b", provider, now.Add(-time.Minute), now.Add(24*time.Hour)),
						resetFirstAuth("c", provider, now, now.Add(2*24*time.Hour)),
					},
					want: []string{"b", "c", "a"},
				},
				{
					name: "known five-hour reset outranks unknown even without weekly reset",
					auths: []*Auth{
						resetFirstAuth("a", provider, time.Time{}, now.Add(time.Hour)),
						resetFirstAuth("b", provider, now.Add(4*time.Hour), time.Time{}),
					},
					want: []string{"b", "a"},
				},
				{
					name: "known weekly reset outranks unknown or elapsed then ID breaks ties",
					auths: []*Auth{
						resetFirstAuth("b", provider, now.Add(time.Hour), time.Time{}),
						resetFirstAuth("a", provider, now.Add(time.Hour), now.Add(-time.Minute)),
						resetFirstAuth("c", provider, now.Add(time.Hour), now.Add(24*time.Hour)),
					},
					want: []string{"c", "a", "b"},
				},
			} {
				t.Run(tc.name, func(t *testing.T) {
					ordered := orderByQuotaReset(tc.auths, now)
					ids := make([]string, len(ordered))
					for i, auth := range ordered {
						ids[i] = auth.ID
					}
					if !reflect.DeepEqual(ids, tc.want) {
						t.Fatalf("order = %v, want %v", ids, tc.want)
					}
				})
			}
		})
	}
}

func TestQuotaResetsAt_CodexAbsoluteAndRelative(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 20, 2, 0, 0, 0, time.UTC)
	observed := now.Add(-10 * time.Minute)
	for _, tc := range []struct {
		name       string
		observedAt time.Time
		signals    map[string]string
		want       quotaResetWindows
	}{
		{
			name:       "relative durations use the observation time for both windows",
			observedAt: observed,
			signals: map[string]string{
				"X-Codex-Primary-Reset-After-Seconds":   "3600",
				"X-Codex-Secondary-Reset-After-Seconds": "604800",
			},
			want: quotaResetWindows{fiveHour: observed.Add(time.Hour), weekly: observed.Add(7 * 24 * time.Hour)},
		},
		{
			name:       "absolute timestamps override relative durations for both windows",
			observedAt: observed,
			signals: map[string]string{
				"X-Codex-Primary-Reset-At":              strconv.FormatInt(now.Add(2*time.Hour).Unix(), 10),
				"X-Codex-Primary-Reset-After-Seconds":   "3600",
				"X-Codex-Secondary-Reset-At":            strconv.FormatInt(now.Add(2*24*time.Hour).Unix(), 10),
				"X-Codex-Secondary-Reset-After-Seconds": "604800",
			},
			want: quotaResetWindows{fiveHour: now.Add(2 * time.Hour), weekly: now.Add(2 * 24 * time.Hour)},
		},
		{
			name: "relative durations without an observation are unknown",
			signals: map[string]string{
				"X-Codex-Primary-Reset-After-Seconds":   "3600",
				"X-Codex-Secondary-Reset-After-Seconds": "604800",
			},
		},
		{
			name:       "invalid absolute timestamps fall back independently",
			observedAt: observed,
			signals: map[string]string{
				"X-Codex-Primary-Reset-At":              "invalid",
				"X-Codex-Primary-Reset-After-Seconds":   "3600",
				"X-Codex-Secondary-Reset-After-Seconds": "-1",
			},
			want: quotaResetWindows{fiveHour: observed.Add(time.Hour)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := &Auth{Provider: "codex", Quota: QuotaState{ObservedAt: tc.observedAt, Signals: tc.signals}}
			if got := quotaResetsAt(auth, now); !got.fiveHour.Equal(tc.want.fiveHour) || !got.weekly.Equal(tc.want.weekly) {
				t.Fatalf("quotaResetsAt() = %+v, want %+v", got, tc.want)
			}
		})
	}

	other := resetFirstAuth("other", "claude", now.Add(time.Hour), now.Add(24*time.Hour))
	other.Provider = "gemini"
	if got := quotaResetsAt(other, now); got != (quotaResetWindows{}) {
		t.Fatalf("unsupported provider has reset times: %+v", got)
	}
}

func TestResetFirstSessionAffinity_PreservesBindingAcrossResetChanges(t *testing.T) {
	t.Parallel()

	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			now := time.Now().Truncate(time.Second)
			a := resetFirstAuth("a", provider, now.Add(30*time.Minute), now.Add(5*24*time.Hour))
			b := resetFirstAuth("b", provider, now.Add(2*time.Hour), now.Add(24*time.Hour))
			auths := []*Auth{a, b}
			selector := NewSessionAffinitySelector(&ResetFirstSelector{})
			defer selector.Stop()
			pick := func(session, wantID string) {
				t.Helper()
				opts := cliproxyexecutor.Options{Metadata: map[string]any{
					cliproxyexecutor.DerivedSessionIDMetadataKey: session,
				}}
				got, err := selector.Pick(context.Background(), provider, "model", opts, auths)
				if err != nil {
					t.Fatalf("Pick(%q): %v", session, err)
				}
				if got == nil || got.ID != wantID {
					t.Fatalf("Pick(%q) = %+v, want %q", session, got, wantID)
				}
			}

			pick("existing-thread", "a")
			a.Quota = resetFirstAuth("a", provider, now.Add(3*time.Hour), now.Add(6*24*time.Hour)).Quota
			pick("existing-thread", "a")
			pick("new-thread", "b")
			a.Quota = resetFirstAuth("a", provider, now.Add(-time.Minute), now.Add(-time.Minute)).Quota
			pick("existing-thread", "a")
			a.Disabled = true
			pick("existing-thread", "b")
			a.Disabled = false
			a.Quota = resetFirstAuth("a", provider, now.Add(15*time.Minute), now.Add(24*time.Hour)).Quota
			pick("existing-thread", "b")
			pick("another-new-thread", "a")
		})
	}
}
