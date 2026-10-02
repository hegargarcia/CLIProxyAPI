package auth

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// ResetFirstSelector prefers the credential whose weekly quota window resets soonest.
//
// The goal is to burn as much of each weekly allowance as possible: usage spent on a
// credential whose week is about to roll over is "refunded" first, so it is drained ahead
// of credentials with a distant weekly reset. Short-window (5h) exhaustion is not part of
// the ordering; it is handled by the normal cooldown, which makes the credential
// unavailable until its 5h reset so requests fail over to the next credential meanwhile.
// The reset time comes from the passive quota snapshot recorded on each credential from
// upstream response headers (see QuotaState.Signals). Credentials with no usable signal,
// or whose last observed reset has already passed, sort after every credential with a
// known upcoming reset. Ties and unknowns fall back to ID order so the result stays
// deterministic.
//
// Like FillFirstSelector it only ever picks within the highest available priority tier.
type ResetFirstSelector struct{}

// Pick selects the available auth with the earliest upcoming quota reset.
func (s *ResetFirstSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := time.Now()
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	ordered := orderByQuotaReset(available, now)
	// Only cold picks reach here when session affinity is on, so info level stays quiet
	// enough while still letting operators confirm the ordering from the journal.
	if entry := selectorLogEntry(ctx); entry != nil && len(ordered) > 1 {
		entry.Infof("reset-first: ordered candidates | provider=%s model=%s order=%s", provider, model, describeQuotaResetOrder(ordered, now))
	}
	return ordered[0], nil
}

// orderByQuotaReset returns a copy of auths sorted by upcoming reset time (soonest first),
// with unknown or elapsed resets last and ID as the final tiebreaker.
func orderByQuotaReset(auths []*Auth, now time.Time) []*Auth {
	ordered := append([]*Auth(nil), auths...)
	resets := make(map[string]time.Time, len(ordered))
	for _, auth := range ordered {
		if auth == nil {
			continue
		}
		if resetAt, ok := quotaResetAt(auth, now); ok {
			resets[auth.ID] = resetAt
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		left, right := ordered[i], ordered[j]
		leftReset, leftKnown := resets[left.ID]
		rightReset, rightKnown := resets[right.ID]
		switch {
		case leftKnown && !rightKnown:
			return true
		case !leftKnown && rightKnown:
			return false
		case leftKnown && rightKnown && !leftReset.Equal(rightReset):
			return leftReset.Before(rightReset)
		default:
			return left.ID < right.ID
		}
	})
	return ordered
}

// quotaResetAt extracts the upcoming weekly-window reset for a credential from its
// observed quota signals. It reports false when no signal is present, the value cannot
// be parsed, or the reset is not in the future.
func quotaResetAt(auth *Auth, now time.Time) (time.Time, bool) {
	if auth == nil || len(auth.Quota.Signals) == 0 {
		return time.Time{}, false
	}
	signals := auth.Quota.Signals
	var resetAt time.Time
	var ok bool
	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "claude":
		// Anthropic's unified 7d window is the weekly allowance.
		resetAt, ok = parseQuotaResetTimestamp(signals["Anthropic-Ratelimit-Unified-7d-Reset"])
	case "codex":
		// Codex's secondary window is the weekly one (primary is 5h). Reset-At is
		// absolute, Reset-After-Seconds is relative to when the snapshot was observed.
		resetAt, ok = parseQuotaResetTimestamp(signals["X-Codex-Secondary-Reset-At"])
		if !ok && !auth.Quota.ObservedAt.IsZero() {
			if seconds, errParse := strconv.ParseInt(strings.TrimSpace(signals["X-Codex-Secondary-Reset-After-Seconds"]), 10, 64); errParse == nil && seconds >= 0 {
				resetAt, ok = auth.Quota.ObservedAt.Add(time.Duration(seconds)*time.Second), true
			}
		}
	}
	if !ok || !resetAt.After(now) {
		return time.Time{}, false
	}
	return resetAt, true
}

// parseQuotaResetTimestamp accepts unix seconds (optionally fractional), unix milliseconds,
// or RFC 3339, matching the formats the Claude rate-limit helper already tolerates.
func parseQuotaResetTimestamp(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if seconds, errParse := strconv.ParseFloat(raw, 64); errParse == nil {
		if seconds <= 0 {
			return time.Time{}, false
		}
		// Anything beyond year ~5138 in seconds is really milliseconds.
		if seconds > 99_999_999_999 {
			return time.UnixMilli(int64(seconds)), true
		}
		whole := int64(seconds)
		return time.Unix(whole, int64((seconds-float64(whole))*1e9)), true
	}
	if parsed, errParse := time.Parse(time.RFC3339, raw); errParse == nil {
		return parsed, true
	}
	return time.Time{}, false
}

// describeQuotaResetOrder renders the sorted candidates as "id(reset in 1h2m3s), id(unknown)".
func describeQuotaResetOrder(ordered []*Auth, now time.Time) string {
	parts := make([]string, 0, len(ordered))
	for _, auth := range ordered {
		if auth == nil {
			continue
		}
		if resetAt, ok := quotaResetAt(auth, now); ok {
			parts = append(parts, auth.ID+"(reset in "+resetAt.Sub(now).Truncate(time.Second).String()+")")
			continue
		}
		parts = append(parts, auth.ID+"(unknown)")
	}
	return strings.Join(parts, ", ")
}
