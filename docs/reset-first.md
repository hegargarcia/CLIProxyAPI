# Reset-first routing

Set `routing.strategy` to `reset-first` to use up each account's weekly quota
before it resets. Weekly quota left at a reset is lost, while a 5-hour limit only
delays work, so the weekly reset decides first.

Routing selects among the highest-priority available, eligible accounts. It
orders them by the earliest known future weekly reset, then uses the 5-hour reset
to break ties. Account ID breaks any remaining tie.

For either window, an unknown, invalid, or elapsed reset sorts after a known
future reset. If both accounts have unknown weekly resets, the 5-hour reset decides.

For example, account A resets in 30 minutes and 5 days; account B resets in
2 hours and tomorrow. B wins because its weekly reset is earlier.

An account that reaches either limit goes into cooldown until the reset reported
by upstream, so new work moves to the next account in order. Accounts without a
5-hour window are ordered by their weekly reset alone.

Reset data comes from passive observations of upstream responses, so an account
only reports new resets when it serves traffic. A Claude or Codex account with no
known future reset in either window, such as one that has not served a request
since the proxy started or whose windows have both elapsed, sorts first. Its next
cold pick refreshes its resets, after which it is ordered normally. Providers
without reset signals sort last. The ordering log marks these accounts `probe`.

Claude uses `Anthropic-Ratelimit-Unified-5h-Reset` and
`Anthropic-Ratelimit-Unified-7d-Reset`. Claude may omit a window's headers, and
some accounts have no 5-hour window; a missing window counts as unknown.

Codex uses `X-Codex-Primary-Reset-At` and `X-Codex-Secondary-Reset-At`, falling
back to each window's `Reset-After-Seconds` when the absolute timestamp cannot be
parsed. Relative durations are anchored to the quota observation time. Each
window's `Window-Minutes` decides whether it is the 5-hour or weekly window, since
some plans report the weekly window as primary. A window longer than a day is
weekly, and a zero-minute window is ignored. Without `Window-Minutes`, primary is
the 5-hour window and secondary the weekly one.

Enable `routing.session-affinity` to preserve an established thread's account as
quota ordering changes. An unavailable account may fail over. Existing priority,
WebSocket eligibility, and cooldown rules remain. Parent-session affinity still
applies when enabled.

The fleet's idle session TTL is 24 hours. Bindings live in memory; restarting the
proxy clears them.
