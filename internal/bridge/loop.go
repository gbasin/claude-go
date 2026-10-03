package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"
)

// Bridge is the `claude remote-control` server skeleton (mod_1269.js): it
// registers an environment, long-polls for work, and heartbeats live
// sessions. Session spawning itself is delegated — the loop is faithful to
// the control plane; what a work item becomes is a SpawnWork concern.
type Bridge struct {
	API     *Client
	Env     *RegisterParams
	Pacing  Pacing
	Spawner Spawner

	// PreserveOnShutdown mirrors real's preserve-on-shutdown: skip archive+
	// deregister so sessions stay resumable by the next `claude remote-control`.
	PreserveOnShutdown bool

	// ResumeSessionID (--session-id): after registration, real re-queues
	// the named session onto this environment via bridge/reconnect.
	ResumeSessionID string

	Log      func(msg string) // user-facing status lines
	LogError func(msg string) // user-facing errors
	OnDebug  func(string)     // [bridge:*] debug lines

	mu            sync.Mutex
	envID         string
	envSecret     string
	secretRenewAt int64                   // unix ms; 0 = no proactive renewal scheduled
	sessions      map[string]*WorkSession // cse_ session id -> work
}

// WorkSession is a live work item: session id -> work id + the bearer used
// on the env-secret lane for that session's heartbeat (real uses the
// session's access token; the work secret's ingress token fills that role).
type WorkSession struct {
	SessionID   string // cse_*
	WorkID      string
	AccessToken string // bearer for heartbeatWork
	SpawnedAt   time.Time
}

// Spawner turns an accepted work item into a running session. Returning a
// nil session means the work was refused; real stops such work items with
// force=false after a base delay — we stop immediately, force=false.
type Spawner interface {
	// Spawn is called after the work secret is decoded and the work is
	// ack'd. sessionSecret carries api_base_url + session_ingress_token.
	Spawn(ctx context.Context, w WorkItem, secret *WorkSecret) (*WorkSession, error)
}

// SpawnerFunc adapter.
type SpawnerFunc func(ctx context.Context, w WorkItem, secret *WorkSecret) (*WorkSession, error)

func (f SpawnerFunc) Spawn(ctx context.Context, w WorkItem, secret *WorkSecret) (*WorkSession, error) {
	return f(ctx, w, secret)
}

func (b *Bridge) debugf(f string, a ...any) {
	if b.OnDebug != nil {
		b.OnDebug(fmt.Sprintf(f, a...))
	}
}
func (b *Bridge) logf(f string, a ...any) {
	if b.Log != nil {
		b.Log(fmt.Sprintf(f, a...))
	}
}
func (b *Bridge) errf(f string, a ...any) {
	if b.LogError != nil {
		b.LogError(fmt.Sprintf(f, a...))
	}
}

func (b *Bridge) pacing() Pacing {
	if b.Pacing.ConnInitialMs == 0 {
		return DefaultPacing()
	}
	return b.Pacing
}

// heartbeatOnce mirrors ce(): one heartbeat pass over every active session.
// 401/403 on a session's own token means its credential lapsed — real
// re-queues it through bridge/reconnect instead of killing the session.
func (b *Bridge) heartbeatOnce(ctx context.Context) string {
	var authFailed []string
	fatal := false
	succeeded := false
	b.mu.Lock()
	sessions := make([]*WorkSession, 0, len(b.sessions))
	for _, s := range b.sessions {
		sessions = append(sessions, s)
	}
	b.mu.Unlock()
	for _, s := range sessions {
		if s.WorkID == "" || s.AccessToken == "" {
			continue
		}
		_, err := b.API.HeartbeatWork(ctx, b.envID, s.WorkID, s.AccessToken)
		if err == nil {
			succeeded = true
			continue
		}
		b.debugf("[bridge:heartbeat] Failed for sessionId=%s workId=%s: %v", s.SessionID, s.WorkID, err)
		var fe *FatalError
		if errors.As(err, &fe) && (fe.Status == 401 || fe.Status == 403) {
			authFailed = append(authFailed, s.SessionID)
		} else if _, fatal2 := fatalStatus(err); fatal2 {
			fatal = true
		}
	}
	for _, sid := range authFailed {
		b.logf("Session %s token expired — re-queuing via bridge/reconnect", sid)
		if err := b.API.ReconnectSession(ctx, b.envID, sid); err != nil {
			if envGone(err) {
				b.debugf("[bridge:heartbeat] reconnectSession(%s) skipped — resource gone: %v", sid, err)
				continue
			}
			b.errf("Failed to refresh session %s token: %v", sid, err)
			b.debugf("[bridge:heartbeat] reconnectSession(%s) failed: %v", sid, err)
		} else {
			b.debugf("[bridge:heartbeat] Re-queued sessionId=%s via bridge/reconnect", sid)
		}
	}
	switch {
	case fatal:
		return "fatal"
	case len(authFailed) > 0:
		return "auth_failed"
	case succeeded:
		return "ok"
	default:
		return "failed"
	}
}

// heartbeatLoop runs heartbeatOnce on the configured cadence. Interval:
// non_exclusive_heartbeat_interval_ms when >0, else 60s once a heartbeat
// has succeeded (real's `St>0 ? 60000` fallback), else disabled.
func (b *Bridge) heartbeatLoop(ctx context.Context, cfg PollConfig) {
	interval := cfg.NonExclusiveHeartbeatIntervalMs
	lastOK := int64(0)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		effective := interval
		if effective <= 0 && lastOK > 0 {
			effective = 60000
		}
		if effective <= 0 {
			effective = 60000 // keep the loop alive; heartbeatOnce no-ops w/o sessions
		}
		if !sleepCtx(ctx, time.Duration(effective)*time.Millisecond) {
			return
		}
		b.mu.Lock()
		n := len(b.sessions)
		b.mu.Unlock()
		if n == 0 {
			continue
		}
		outcome := b.heartbeatOnce(ctx)
		switch outcome {
		case "ok":
			lastOK = time.Now().UnixMilli()
		case "auth_failed", "fatal":
			// reconnect already attempted inside heartbeatOnce; real waits
			// for the next cycle rather than hot-looping.
		}
	}
}

// handleWork processes one poll row: decode the secret, ack, spawn.
// Mirrors the work-pickup path in mod_1269.js.
func (b *Bridge) handleWork(ctx context.Context, w *WorkItem) {
	var wd WorkData
	json.Unmarshal(w.Data, &wd)
	if wd.Mode == "tool_use" {
		// tool-host mode is handled by a dedicated lane in real; without a
		// toolHostHandler we stop the item like real's unsupported-mode path.
		b.debugf("[bridge:work] Stopping work item with an unsupported mode workId=%s sessionId=%s", w.ID, wd.ID)
		b.API.StopWork(ctx, b.envID, w.ID, b.envSecret, false)
		return
	}
	secret, err := DecodeWorkSecret(w.Secret)
	if err != nil {
		b.errf("Failed to decode work secret for workId=%s: %v", w.ID, err)
		b.API.StopWork(ctx, b.envID, w.ID, b.envSecret, false)
		return
	}
	// Acknowledge: bearer is the work's own session ingress token.
	if err := b.API.AcknowledgeWork(ctx, b.envID, w.ID, secret.SessionIngressToken); err != nil {
		b.debugf("[bridge:work] Acknowledge failed workId=%s: %v", w.ID, err)
	}
	var sess *WorkSession
	if b.Spawner != nil {
		sess, err = b.Spawner.Spawn(ctx, *w, secret)
		if err != nil {
			b.errf("Session spawn failed for workId=%s: %v", w.ID, err)
			b.API.StopWork(ctx, b.envID, w.ID, b.envSecret, true)
			return
		}
	}
	if sess == nil {
		sess = &WorkSession{SessionID: wd.ID}
	}
	if sess.SessionID == "" {
		sess.SessionID = wd.ID
	}
	if sess.WorkID == "" {
		sess.WorkID = w.ID
	}
	if sess.AccessToken == "" {
		sess.AccessToken = secret.SessionIngressToken
	}
	sess.SpawnedAt = time.Now()
	b.mu.Lock()
	b.sessions[sess.SessionID] = sess
	b.mu.Unlock()
	b.debugf("[bridge:work] Picked up workId=%s sessionId=%s", w.ID, sess.SessionID)
}

// reregister mirrors MLn's skeleton: re-register in place with
// reuseEnvironmentId, then re-queue every active session through
// bridge/reconnect (tQt), up to eQt attempts each.
// Returns "reregistered" | "replaced" (fatal) | error.
func (b *Bridge) reregister(ctx context.Context, attempt int, trigger string) (string, error) {
	b.debugf("[bridge:poll] Re-registering environment %s (attempt %d/%d, trigger=%s)",
		b.envID, attempt, ReRegisterMaxAttempts, trigger)
	p := *b.Env
	p.ReuseEnvironmentID = b.envID
	res, err := b.API.RegisterBridgeEnvironment(ctx, &p)
	if err != nil {
		if st, ok := fatalStatus(err); ok {
			_ = st
			return "fatal", err
		}
		return "transient", err
	}
	if res.EnvironmentID != b.envID {
		b.debugf("[bridge:poll] Re-registration returned %s, not %s; not adopting it",
			res.EnvironmentID, b.envID)
		b.API.DeregisterEnvironment(ctx, res.EnvironmentID)
		return "replaced", fmt.Errorf(
			"Remote Control could not renew this environment's credential: the server issued a new environment instead. Restart `claude remote-control` to start fresh.")
	}
	b.mu.Lock()
	b.envSecret = res.EnvironmentSecret
	if res.EnvironmentSecretExpiresIn > 0 {
		b.secretRenewAt = SecretRenewalAt(res.EnvironmentSecretExpiresIn,
			time.Now().UnixMilli(), rand.Float64())
	}
	ids := make([]string, 0, len(b.sessions))
	for id := range b.sessions {
		ids = append(ids, id)
	}
	b.mu.Unlock()
	// re-queue active sessions (tQt): rejected => done, transient => retry
	// budget eQt, then left to token refresh.
	for _, sid := range ids {
		requeueSession := func() error {
			var err error
			for i := 0; i < RequeueMaxAttempts; i++ {
				if err = b.API.ReconnectSession(ctx, b.envID, sid); err == nil {
					return nil
				}
				if _, fatal := fatalStatus(err); fatal {
					return nil // rejected — nothing more to do
				}
				if ctx.Err() != nil {
					return errAborted
				}
			}
			b.debugf("[bridge:poll] Giving up re-queuing sessionId=%s after %d attempts; left to token refresh",
				sid, RequeueMaxAttempts)
			return nil
		}
		if err := requeueSession(); err != nil && err != errAborted {
			return "transient", err
		}
	}
	b.debugf("[bridge:poll] Environment %s re-registered in place; re-queued %d session(s)",
		b.envID, len(ids))
	return "reregistered", nil
}

// secretDue mirrors Wt(): proactive renewal when the env secret's scheduled
// refresh time has passed.
func (b *Bridge) secretDue() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.secretRenewAt > 0 && time.Now().UnixMilli() >= b.secretRenewAt
}

// Run is the poll loop. It registers the environment, then cycles
// poll -> work-pickup -> heartbeat until ctx is done or a fatal error
// lands. On clean shutdown it archives sessions and deregisters unless
// PreserveOnShutdown.
func (b *Bridge) Run(ctx context.Context) error {
	res, err := b.API.RegisterBridgeEnvironment(ctx, b.Env)
	if err != nil {
		return fmt.Errorf("Bridge registration failed: %w", err)
	}
	b.mu.Lock()
	b.envID = res.EnvironmentID
	b.envSecret = res.EnvironmentSecret
	b.sessions = map[string]*WorkSession{}
	if res.EnvironmentSecretExpiresIn > 0 {
		b.secretRenewAt = SecretRenewalAt(res.EnvironmentSecretExpiresIn,
			time.Now().UnixMilli(), rand.Float64())
	}
	b.mu.Unlock()

	// --session-id resume: re-queue the session onto this environment.
	if b.ResumeSessionID != "" {
		if err := b.API.ReconnectSession(ctx, b.envID, b.ResumeSessionID); err != nil {
			b.errf("Session %s could not be re-queued: %v", b.ResumeSessionID, err)
		} else {
			b.logf("Resuming session %s on environment %s", b.ResumeSessionID, b.envID)
		}
	}

	hbCtx, stopHB := context.WithCancel(ctx)
	defer stopHB()
	go b.heartbeatLoop(hbCtx, LoadPollConfig())

	pacing := b.pacing()
	connFailures := 0
	disconnectedSince := time.Now()
	reregisterAttempts := 0

	defer b.shutdown(ctx)

	for ctx.Err() == nil {
		cfg := LoadPollConfig()
		if b.secretDue() {
			outcome, rerr := b.reregister(ctx, reregisterAttempts+1, "proactive")
			switch outcome {
			case "reregistered":
				reregisterAttempts = 0
			case "transient":
				reregisterAttempts++
				_ = rerr
			default: // fatal|replaced
				return rerr
			}
			continue
		}
		b.mu.Lock()
		active := len(b.sessions)
		b.mu.Unlock()
		work, err := b.API.PollForWork(ctx, b.envID, b.envSecret,
			int64(cfg.ReclaimOlderThanMs), cfg.pollIntervalFor(active, b.Env.MaxSessions))
		if err == nil {
			if connFailures > 0 {
				b.logf("Remote Control reconnected after %s", time.Since(disconnectedSince).Round(time.Second))
				b.debugf("[bridge:poll] Reconnected after %s", time.Since(disconnectedSince))
			}
			connFailures = 0
			reregisterAttempts = 0
			if work == nil {
				continue
			}
			b.handleWork(ctx, work)
			continue
		}
		if connFailures == 0 {
			disconnectedSince = time.Now()
		}
		// classify: env gone -> re-register; 401 -> refresh+re-register;
		// other fatal -> exit; else transient backoff.
		if envGone(err) {
			if active > 0 && reregisterAttempts < ReRegisterMaxAttempts {
				reregisterAttempts++
				outcome, rerr := b.reregister(ctx, reregisterAttempts, "poll_404")
				if outcome == "reregistered" {
					reregisterAttempts = 0
					connFailures = 0
					continue
				}
				if outcome == "replaced" || outcome == "fatal" {
					return rerr
				}
			} else {
				b.errf("%s", reconnectHint(err.Error()))
				return err
			}
		}
		var fe *FatalError
		if errors.As(err, &fe) {
			if fe.Status == 401 {
				if reregisterAttempts < ReRegisterMaxAttempts {
					reregisterAttempts++
					outcome, rerr := b.reregister(ctx, reregisterAttempts, "poll_401")
					if outcome == "reregistered" {
						reregisterAttempts = 0
						connFailures = 0
						continue
					}
					if outcome == "replaced" || outcome == "fatal" {
						return rerr
					}
				} else {
					return fmt.Errorf("Remote Control could not renew this environment's credential after repeated attempts. Restart `claude remote-control` to start fresh.")
				}
			}
			b.errf("%s", reconnectHint(fe.Message))
			return fe
		}
		connFailures++
		if time.Since(disconnectedSince) > pacing.giveUpAfter() {
			b.errf("%s", reconnectHint(fmt.Sprintf("Remote Control could not reach the server after %s",
				time.Since(disconnectedSince).Round(time.Second))))
			return err
		}
		delay := pacing.NextBackoff(connFailures)
		if re, ok := err.(*RateLimitError); ok && re.RetryAfterMs > 0 {
			if ra := time.Duration(re.RetryAfterMs) * time.Millisecond; ra > delay {
				delay = ra
			}
		}
		if !sleepCtx(ctx, delay) {
			break
		}
	}
	return ctx.Err()
}

// shutdown archives sessions + deregisters the environment, or preserves
// them for resume (real's preserveOnShutdown branch).
func (b *Bridge) shutdown(ctx context.Context) {
	b.mu.Lock()
	ids := make([]string, 0, len(b.sessions))
	for id := range b.sessions {
		ids = append(ids, id)
	}
	b.mu.Unlock()
	if b.PreserveOnShutdown {
		if len(ids) > 0 {
			b.logf("Environment preserved. Restart `claude remote-control` to reconnect existing sessions.")
			b.debugf("[bridge:shutdown] Skipping archive+deregister to allow resume (env %s)", b.envID)
		}
		return
	}
	// teardown_archive_timeout_ms applies per archive in real; use a short
	// detached context since ctx may already be done.
	tctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	for _, sid := range ids {
		if err := b.API.ArchiveSession(tctx, sid); err != nil {
			b.debugf("[bridge:shutdown] Archive of session %s failed: %v", sid, err)
		}
	}
	if err := b.API.MarkEnvironmentOffline(tctx, b.envID, b.envSecret); err != nil {
		b.debugf("[bridge:shutdown] MarkOffline failed: %v", err)
	}
	if err := b.API.DeregisterEnvironment(tctx, b.envID); err != nil {
		b.debugf("[bridge:shutdown] Deregister failed: %v", err)
	}
}
