package session

import (
	"context"
	"sync"
	"time"

	"github.com/opendray/opendray-v2/internal/eventbus"
)

// tryRateLimitFailover scans the session's rolling output window for
// the Claude rate-limit banner and, on a match, marks the current
// account throttled and switches the session to the next available
// account. Called from pumpStdout via the rate-limit-scan tick (at
// most once per rateLimitScanCooldown per session).
//
// Failure modes (all non-fatal — we log and let the session keep
// running on the throttled account):
//
//   - resolver nil OR autoFailover disabled → never called (gated upstream)
//   - banner not found → silent return
//   - already-throttled current account → silent return (don't re-fire)
//   - no failover target available → log warn, publish "no_target" event
//   - SwitchClaudeAccount itself fails → log warn, publish "failover_failed" event
//
// MVP scope: only sessions pinned to a NAMED account get failover.
// Sessions running on the empty-id default (~/.claude) are skipped
// here because mapping the default to a real account row would
// require an extra round-trip the resolver doesn't expose yet. The
// majority of sessions on a multi-account install end up pinned at
// spawn time via PickAutoAssignClaudeAccount, so this gap shrinks to
// zero in practice once the operator has ≥2 enabled accounts.
func (m *Manager) tryRateLimitFailover(rs *runningSession, now time.Time) {
	window := rs.rateLimitWindow()
	reset, ok := ScanForRateLimitBanner(window, now)
	if !ok {
		return
	}

	rs.sessMu.RLock()
	sessID := rs.sess.ID
	currentAccountID := rs.sess.ClaudeAccountID
	workDir := rs.sess.EffectiveWorkDir()
	claudeID := rs.sess.ClaudeSessionID
	rs.sessMu.RUnlock()

	// One attempt per session at a time: the regular scan, the trailing
	// scan and the freshness retries below can otherwise overlap.
	if _, busy := failoverInflight.LoadOrStore(sessID, struct{}{}); busy {
		return
	}
	defer failoverInflight.Delete(sessID)

	// --resume re-renders the old banner under the new account; the same
	// reset instant means it is that echo, not a fresh limit.
	if prev, seen := failoverHandled.Load(sessID); seen && prev.(time.Time).Equal(reset) {
		return
	}

	// The PTY often shows the banner a moment before Claude flushes the
	// matching transcript line, so give the transcript a few seconds.
	fresh := false
	for i := 0; i < limitFreshRetries; i++ {
		if i > 0 {
			time.Sleep(limitFreshRetryDelay)
		}
		if limitIsFresh(m.claudeHistoryCfg, workDir, claudeID, time.Now()) {
			fresh = true
			break
		}
	}
	if !fresh {
		m.log.Info("rate-limit banner seen but transcript has no fresh limit; skipping failover",
			"session", sessID, "current_account", currentAccountID, "reset", reset.Format(time.RFC3339))
		return
	}

	// MVP: don't try to fail over from the empty-id default. The
	// scanner has no clean way to resolve it without another DB hop
	// the resolver interface deliberately doesn't expose.
	if currentAccountID == "" {
		m.log.Warn("rate-limit hit on a session with no pinned account; skipping failover", "session", sessID)
		return
	}

	// The account may already be marked throttled by ANOTHER session
	// that hit the same limit first; this session still has to move.
	// Echo re-fires of this session are stopped by failoverHandled and
	// limitIsFresh above, so a throttled mark is no reason to stay.
	failoverHandled.Store(sessID, reset)
	if !m.claudeAccounts.IsClaudeAccountThrottled(currentAccountID) {
		m.claudeAccounts.MarkClaudeAccountThrottled(currentAccountID, reset)
	}
	m.log.Info("claude rate-limit detected, attempting failover",
		"session", sessID,
		"current_account", currentAccountID,
		"reset", reset.Format(time.RFC3339),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	target, err := m.claudeAccounts.PickFailoverClaudeAccount(ctx, currentAccountID)
	if err != nil {
		m.log.Warn("failover account-pick failed", "session", sessID, "err", err)
		return
	}
	if target == "" {
		m.log.Warn("rate-limit hit but no failover target available",
			"session", sessID,
			"current_account", currentAccountID,
		)
		if m.bus != nil {
			m.bus.Publish(eventbus.Event{
				Topic: "session.auto_failover_no_target",
				Data: map[string]any{
					"session_id": sessID,
					"account_id": currentAccountID,
					"reset":      reset.UTC().Format(time.RFC3339),
				},
			})
		}
		return
	}

	// Automatic throttle failover carries context: a rate-limit bounce
	// is involuntary, so dropping the conversation on the floor is worse
	// than seeding the standby account with a recap. The target is one
	// of the operator's own accounts (same trust domain), and the recap
	// rides the same --append-system-prompt path as a manual carry.
	if _, err := m.SwitchClaudeAccount(ctx, sessID, target, true); err != nil {
		m.log.Warn("auto-failover switch failed",
			"session", sessID,
			"from_account", currentAccountID,
			"to_account", target,
			"err", err,
		)
		if m.bus != nil {
			m.bus.Publish(eventbus.Event{
				Topic: "session.auto_failover_failed",
				Data: map[string]any{
					"session_id":   sessID,
					"from_account": currentAccountID,
					"to_account":   target,
					"err":          err.Error(),
				},
			})
		}
		return
	}

	rs.clearRateLimitWindow()
	go m.continueAfterFailover(sessID)
	m.log.Info("auto-failover succeeded",
		"session", sessID,
		"from_account", currentAccountID,
		"to_account", target,
	)
	if m.bus != nil {
		m.bus.Publish(eventbus.Event{
			Topic: "session.auto_switched",
			Data: map[string]any{
				"session_id":   sessID,
				"from_account": currentAccountID,
				"to_account":   target,
				"reason":       "rate_limit",
				"reset":        reset.UTC().Format(time.RFC3339),
			},
		})
	}
}

// failoverHandled remembers, per session, the reset instant of the banner
// that last triggered a switch.
var failoverHandled sync.Map

// failoverInflight holds the sessions with a failover attempt running.
var failoverInflight sync.Map

const (
	limitFreshRetries    = 6
	limitFreshRetryDelay = 2 * time.Second
)

// failoverContinuePrompt restarts the turn the limit interrupted.
const failoverContinuePrompt = "ادامه بده"

// continueAfterFailover waits for the resumed TUI's input box and submits
// a continue prompt, so the operator doesn't have to notice the switch.
func (m *Manager) continueAfterFailover(sessID string) {
	// Long enough for the operator to answer a first-use trust prompt.
	ctx, cancel := context.WithTimeout(context.Background(), 11*time.Minute)
	defer cancel()
	for i := 0; i < 300; i++ {
		time.Sleep(2 * time.Second)
		screen := m.RecentScreen(sessID)
		if parseScreenPrompt(screen) != nil || !screenInputReady(screen) {
			continue
		}
		time.Sleep(time.Second)
		if err := m.Input(ctx, sessID, []byte("\x1b[200~"+failoverContinuePrompt+"\x1b[201~")); err != nil {
			m.log.Warn("failover continue prompt failed", "session", sessID, "err", err)
			return
		}
		time.Sleep(350 * time.Millisecond)
		if err := m.Input(ctx, sessID, []byte{'\r'}); err != nil {
			m.log.Warn("failover continue submit failed", "session", sessID, "err", err)
		}
		return
	}
	m.log.Warn("failover: resumed session never became ready", "session", sessID)
}
