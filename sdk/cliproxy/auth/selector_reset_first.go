package auth

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

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

type quotaResetWindows struct {
	fiveHour   time.Time
	weekly     time.Time
	needsProbe bool
}

func orderByQuotaReset(auths []*Auth, now time.Time) []*Auth {
	ordered := append([]*Auth(nil), auths...)
	resets := make(map[string]quotaResetWindows, len(ordered))
	for _, auth := range ordered {
		if auth != nil {
			resets[auth.ID] = quotaResetsAt(auth, now)
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		left, right := ordered[i], ordered[j]
		leftReset, rightReset := resets[left.ID], resets[right.ID]
		if leftReset.needsProbe != rightReset.needsProbe {
			return leftReset.needsProbe
		}
		// Weekly quota left at reset is lost; a 5-hour limit only delays it.
		if order := compareQuotaReset(leftReset.weekly, rightReset.weekly); order != 0 {
			return order < 0
		}
		if order := compareQuotaReset(leftReset.fiveHour, rightReset.fiveHour); order != 0 {
			return order < 0
		}
		return left.ID < right.ID
	})
	return ordered
}

func compareQuotaReset(left, right time.Time) int {
	switch {
	case left.IsZero() && right.IsZero():
		return 0
	case left.IsZero():
		return 1
	case right.IsZero():
		return -1
	default:
		return left.Compare(right)
	}
}

// quotaResetsAt marks a Claude or Codex auth with no known future reset as needing a
// probe: only traffic refreshes its passive snapshot, so sorting it last would starve it.
func quotaResetsAt(auth *Auth, now time.Time) quotaResetWindows {
	if auth == nil {
		return quotaResetWindows{}
	}
	signals := auth.Quota.Signals
	var resets quotaResetWindows
	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "claude":
		resets.fiveHour, _ = parseQuotaResetTimestamp(signals["Anthropic-Ratelimit-Unified-5h-Reset"])
		resets.weekly, _ = parseQuotaResetTimestamp(signals["Anthropic-Ratelimit-Unified-7d-Reset"])
	case "codex":
		for _, window := range []string{"Primary", "Secondary"} {
			weekly, present := codexWindowIsWeekly(signals, window)
			if !present {
				continue
			}
			if weekly {
				resets.weekly = codexQuotaResetAt(auth.Quota, window)
			} else {
				resets.fiveHour = codexQuotaResetAt(auth.Quota, window)
			}
		}
	default:
		return quotaResetWindows{}
	}
	if !resets.fiveHour.After(now) {
		resets.fiveHour = time.Time{}
	}
	if !resets.weekly.After(now) {
		resets.weekly = time.Time{}
	}
	resets.needsProbe = resets.fiveHour.IsZero() && resets.weekly.IsZero()
	return resets
}

// codexWindowIsWeekly classifies a Codex window by its advertised length, since some
// plans put the weekly window in the primary slot. Without a length it falls back to
// the usual layout: primary is the short window and secondary the weekly one.
func codexWindowIsWeekly(signals map[string]string, window string) (weekly, present bool) {
	minutes, errParse := strconv.ParseInt(strings.TrimSpace(signals["X-Codex-"+window+"-Window-Minutes"]), 10, 64)
	switch {
	case errParse != nil:
		return window == "Secondary", true
	case minutes <= 0:
		return false, false
	default:
		return minutes > 24*60, true
	}
}

func codexQuotaResetAt(quota QuotaState, window string) time.Time {
	prefix := "X-Codex-" + window
	if resetAt, ok := parseQuotaResetTimestamp(quota.Signals[prefix+"-Reset-At"]); ok {
		return resetAt
	}
	if !quota.ObservedAt.IsZero() {
		if seconds, errParse := strconv.ParseInt(strings.TrimSpace(quota.Signals[prefix+"-Reset-After-Seconds"]), 10, 64); errParse == nil && seconds >= 0 {
			return quota.ObservedAt.Add(time.Duration(seconds) * time.Second)
		}
	}
	return time.Time{}
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

func describeQuotaResetOrder(ordered []*Auth, now time.Time) string {
	parts := make([]string, 0, len(ordered))
	for _, auth := range ordered {
		if auth == nil {
			continue
		}
		resets := quotaResetsAt(auth, now)
		if resets.needsProbe {
			parts = append(parts, auth.ID+"(probe)")
			continue
		}
		parts = append(parts, auth.ID+"(5h="+describeQuotaReset(resets.fiveHour, now)+", weekly="+describeQuotaReset(resets.weekly, now)+")")
	}
	return strings.Join(parts, ", ")
}

func describeQuotaReset(resetAt, now time.Time) string {
	if resetAt.IsZero() {
		return "unknown"
	}
	return "reset in " + resetAt.Sub(now).Truncate(time.Second).String()
}
