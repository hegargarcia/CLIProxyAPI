# Reset-first routing

Set `routing.strategy` to `reset-first` to prefer accounts whose 5-hour quota
window resets soonest.

Routing selects among the highest-priority available, eligible accounts. It
orders them by the earliest known future 5-hour reset, then uses the weekly reset
to break ties. Account ID breaks any remaining tie.

For either window, an unknown, invalid, or elapsed reset sorts after a known
future reset. If both accounts have unknown 5-hour resets, the weekly reset decides.

For example, account A resets in 30 minutes and 5 days; account B resets in
2 hours and tomorrow. A wins because its 5-hour reset is earlier.

Reset data comes from passive observations; routing does not probe accounts.
Claude uses `Anthropic-Ratelimit-Unified-5h-Reset` and
`Anthropic-Ratelimit-Unified-7d-Reset`. Codex uses `X-Codex-Primary-Reset-At` and
`X-Codex-Secondary-Reset-At`, falling back to each window's `Reset-After-Seconds`
when the absolute timestamp cannot be parsed. Relative durations are anchored
to the quota observation time.

Enable `routing.session-affinity` to preserve an established thread's account as
quota ordering changes. An unavailable account may fail over. Existing priority,
WebSocket eligibility, and cooldown rules remain. Parent-session affinity still
applies when enabled.

The fleet's idle session TTL is 24 hours. Bindings live in memory; restarting the
proxy clears them.
