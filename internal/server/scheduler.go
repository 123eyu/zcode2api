package server

import (
	"context"
	rand "math/rand/v2"
	"strings"
	"time"
	accountdata "zcode2api/internal/server/account"
	"zcode2api/internal/server/fault"
	"zcode2api/internal/server/shared"
)

const accountConcurrencyLimit = 3

const maxQueuedRequests = 1024

type accountAssignment struct {
	account    shared.M
	busy       bool
	candidates []any
	policy     string
}

type accountWaiter struct {
	ctx      context.Context
	o        inferOptions
	skip     map[string]bool
	deadline time.Time
	result   chan accountAssignment
}

type schedulingCandidate struct {
	row       shared.M
	remaining float64
	skip      string
}

type schedulingPool struct {
	group    shared.M
	accounts []schedulingCandidate
}

func (a *App) schedulingPools(key shared.M) []schedulingPool {
	query := `SELECT a.*,g.id AS _gid,g.name AS _name,g.role AS _role,g.policy AS _policy,g.family_filter AS _family,g.urgent_window_min AS _urgent FROM groups g LEFT JOIN accounts a ON a.group_id=g.id WHERE g.enabled=1`
	chain, explicit := shared.Parse(key["group_order"], nil).([]any)
	args := []any{}
	if explicit {
		if len(chain) == 0 {
			return nil
		}
		query += " AND g.id IN (" + strings.TrimSuffix(strings.Repeat("?,", len(chain)), ",") + ")"
		args = append(args, chain...)
	}
	query += " ORDER BY g.created_at,g.id,a.created_at,a.id"
	pools := map[string]*schedulingPool{}
	order := []string{}
	for _, v := range a.rows(query, args...) {
		r := shared.Obj(v)
		gid := shared.Str(r["_gid"])
		pool := pools[gid]
		if pool == nil {
			pool = &schedulingPool{group: shared.M{"id": gid, "name": r["_name"], "role": r["_role"], "policy": r["_policy"], "family_filter": r["_family"], "urgent_window_min": r["_urgent"]}}
			pools[gid] = pool
			order = append(order, gid)
		}
		if r["id"] != nil {
			pool.accounts = append(pool.accounts, schedulingCandidate{row: r})
		}
	}
	if !explicit {
		chain = []any{}
		for _, role := range []string{"primary", "backup"} {
			for _, id := range order {
				if shared.Str(pools[id].group["role"]) == role {
					chain = append(chain, id)
					break
				}
			}
		}
	}
	out := make([]schedulingPool, 0, len(chain))
	seen := map[string]bool{}
	for _, id := range chain {
		gid := shared.Str(id)
		if p := pools[gid]; p != nil && !seen[gid] {
			out = append(out, *p)
			seen[gid] = true
		}
	}
	return out
}

func (a *App) selectAccount(o inferOptions, excluded map[string]bool) (shared.M, bool, []any, string) {
	params := a.retryParams()
	now := shared.NowMS()
	minWindow := int64(shared.Num(shared.Fallback(params["min_request_window_sec"], 60)) * 1000)
	pools := a.schedulingPools(o.Key)
	for pi := range pools {
		p := &pools[pi]
		for i := range p.accounts {
			c := &p.accounts[i]
			r := c.row
			switch {
			case excluded[shared.Str(r["id"])]:
				c.skip = "excluded_for_request"
			case shared.Str(r["status"]) != "active":
				c.skip = "status=" + shared.Str(r["status"])
			case accountdata.LoginJWTExpired(a.accountTokens(r)):
				c.skip = "login_expired"
				a.scheduleNext(shared.Str(r["id"]), 0)
			case shared.Truth(r["card_expires_at"]) && shared.Integer(r["card_expires_at"])-now < minWindow:
				c.skip = "card_expiring"
			case shared.Truth(p.group["family_filter"]) && p.group["family_filter"] != r["family"]:
				c.skip = "family_filter"
			}
			if c.skip != "" {
				continue
			}
			caps := shared.Arr(shared.Parse(r["capabilities"], nil))
			if len(caps) > 0 {
				allowed := false
				for _, cap := range caps {
					if strings.EqualFold(shared.Str(cap), o.Model) {
						allowed = true
						break
					}
				}
				if !allowed {
					c.skip = "model_not_available"
					continue
				}
			}
			data := accountdata.BalanceData(r)
			if _, known := data["balances"].([]any); !known {
				c.skip = "quota_unknown"
				continue
			}
			c.remaining = accountdata.RemainingBalance(data, now)
			if !(c.remaining > 0) {
				c.skip = "quota_remaining=0"
			}
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	candidates := []any{}
	busy := false
	for _, p := range pools {
		usable := make([]schedulingCandidate, 0, len(p.accounts))
		for _, c := range p.accounts {
			reason := c.skip
			id := shared.Str(c.row["id"])
			if reason == "" && a.state(id).QuotaExhausted {
				reason = "quota_remaining=0"
			}
			if reason == "" {
				s := a.state(id)
				if s.BusyUntil > now || s.BusyUntil != 0 && (s.BusyProbe || s.Inflight > 0) {
					reason = "upstream_busy"
					busy = true
				}
			}
			candidates = append(candidates, shared.M{"account": id, "label": c.row["label"], "skip": shared.Nullable(reason), "picked": false})
			if reason == "" {
				usable = append(usable, c)
			}
		}
		policy := shared.Str(p.group["policy"])
		sticky := a.sticky[o.Affinity]
		stickyValid := o.Sticky > 0 && float64(now-sticky.At) <= o.Sticky*60000
		urgentWindow := int64(shared.Num(shared.Fallback(p.group["urgent_window_min"], 180)) * 60000)
		preferred := func(x, y schedulingCandidate) bool {
			xs, ys := a.state(shared.Str(x.row["id"])), a.state(shared.Str(y.row["id"]))
			if stickyValid {
				xp, yp := shared.Str(x.row["id"]) == sticky.Account, shared.Str(y.row["id"]) == sticky.Account
				if xp != yp {
					return xp
				}
			}
			switch policy {
			case "remaining_desc":
				return x.remaining > y.remaining
			case "expiring_first":
				xe, ye := shared.Integer(x.row["card_expires_at"]), shared.Integer(y.row["card_expires_at"])
				xu, yu := xe > 0 && xe-now < urgentWindow, ye > 0 && ye-now < urgentWindow
				if xu != yu {
					return xu
				}
				if xu {
					return xe < ye
				}
				return xs.LastUsed > ys.LastUsed
			default:
				if xs.Inflight != ys.Inflight {
					return xs.Inflight < ys.Inflight
				}
				return xs.LastUsed < ys.LastUsed
			}
		}
		var picked *schedulingCandidate
		for i := range usable {
			c := &usable[i]
			id := shared.Str(c.row["id"])
			s := a.state(id)
			if s.Inflight >= accountConcurrencyLimit {
				busy = true
				continue
			}
			if picked == nil || preferred(*c, *picked) {
				picked = c
			}
		}
		if picked != nil {
			id := shared.Str(picked.row["id"])
			a.state(id).Inflight++
			picked.row["_busy_generation"] = a.state(id).BusyGeneration
			picked.row["_busy_probe"] = a.state(id).BusyUntil != 0
			if a.state(id).BusyUntil != 0 {
				a.state(id).BusyProbe = true
			}
			for _, v := range candidates {
				if m := shared.Obj(v); shared.Str(m["account"]) == id {
					m["picked"] = true
				}
			}
			return picked.row, busy, candidates, policy
		}
	}
	return nil, busy, candidates, ""
}

func (a *App) availability() <-chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.available == nil {
		a.available = make(chan struct{})
	}
	return a.available
}

func (a *App) notifyAvailableLocked() {
	if a.available != nil {
		close(a.available)
	}
	a.available = make(chan struct{})
}

func (a *App) notifyAvailable() { a.mu.Lock(); a.notifyAvailableLocked(); a.mu.Unlock() }

func (a *App) acquire(ctx context.Context, o inferOptions, skip map[string]bool, limit time.Duration) (shared.M, bool, []any, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, nil, "", err
	}
	w := &accountWaiter{ctx: ctx, o: o, skip: skip, result: make(chan accountAssignment, 1)}
	if limit >= 0 {
		w.deadline = time.Now().Add(limit)
	}
	a.scheduleMu.Lock()
	if len(a.waiters) >= maxQueuedRequests {
		a.scheduleMu.Unlock()
		return nil, false, nil, "", &fault.Failure{Kind: "busy", Code: "queue_full", Message: "请求等待队列已满", RetryAfter: time.Second}
	}
	a.waiters = append(a.waiters, w)
	a.scheduleMu.Unlock()
	a.notifyAvailable()
	defer func() {
		a.scheduleMu.Lock()
		for i, pending := range a.waiters {
			if pending == w {
				copy(a.waiters[i:], a.waiters[i+1:])
				a.waiters[len(a.waiters)-1] = nil
				a.waiters = a.waiters[:len(a.waiters)-1]
				break
			}
		}
		// Cancellation can race with a dispatch that already reserved a slot.
		select {
		case assigned := <-w.result:
			if assigned.account != nil {
				a.finishAccountAttempt(assigned.account, nil, false, nil)
			}
		default:
		}
		a.scheduleMu.Unlock()
		a.notifyAvailable()
	}()
	select {
	case <-ctx.Done():
		return nil, false, nil, "", ctx.Err()
	case <-a.ctx.Done():
		return nil, false, nil, "", a.ctx.Err()
	case assigned := <-w.result:
		return assigned.account, assigned.busy, assigned.candidates, assigned.policy, nil
	}
}

// One dispatcher owns wakeups and cooldown timers for the entire queue.
func (a *App) runAccountQueue() {
	for a.ctx.Err() == nil {
		changed := a.availability()
		a.dispatchAccounts()
		a.scheduleMu.Lock()
		queued := len(a.waiters) > 0
		delay := a.nextBusyDelay()
		for _, w := range a.waiters {
			if !w.deadline.IsZero() {
				delay = min(delay, max(0, time.Until(w.deadline)))
			}
			if !w.o.RecheckAt.IsZero() {
				delay = min(delay, max(0, time.Until(w.o.RecheckAt)))
			}
		}
		a.scheduleMu.Unlock()
		var timer *time.Timer
		var tick <-chan time.Time
		if queued {
			timer = time.NewTimer(delay)
			tick = timer.C
		}
		select {
		case <-a.ctx.Done():
		case <-changed:
		case <-tick:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

// Serve older compatible waiters first; a blocked key must not block other pools.
func (a *App) dispatchAccounts() {
	a.scheduleMu.Lock()
	defer a.scheduleMu.Unlock()
	pending := a.waiters[:0]
	for _, w := range a.waiters {
		if w.ctx.Err() != nil {
			continue
		}
		account, busy, candidates, policy := a.selectAccount(w.o, w.skip)
		if account == nil {
			for _, candidate := range candidates {
				if shared.Obj(candidate)["skip"] == "upstream_busy" {
					w.deadline = time.Time{}
					break
				}
			}
		}
		// Unlike the ordinary busy wait, a throttle recheck cannot be waived
		// by upstream concurrency. Return to infer so it can rebuild the skips.
		now := time.Now()
		recheck := !w.o.RecheckAt.IsZero() && !now.Before(w.o.RecheckAt)
		if account != nil || !busy || recheck || !w.deadline.IsZero() && !now.Before(w.deadline) {
			w.result <- accountAssignment{account, busy, candidates, policy}
		} else {
			pending = append(pending, w)
		}
	}
	clear(a.waiters[len(pending):])
	a.waiters = pending
}

func (a *App) nextBusyDelay() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	delay := time.Second
	for _, s := range a.states {
		if s.BusyUntil > shared.NowMS() {
			delay = min(delay, time.Duration(s.BusyUntil-shared.NowMS())*time.Millisecond)
		}
	}
	return max(time.Millisecond, delay)
}

func (a *App) admitAccount(account shared.M) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.state(shared.Str(account["id"]))
	if s.BusyGeneration == shared.Integer(account["_busy_generation"]) {
		s.BusyUntil, s.BusyProbe, s.BusyBackoffStep = 0, false, 0
		a.notifyAvailableLocked()
	}
}

// Publish upstream busy state before waking other requests on slot release.
func (a *App) finishAccountAttempt(account shared.M, failure *fault.Failure, completed bool, params shared.M) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.state(shared.Str(account["id"]))
	s.Inflight = max(0, s.Inflight-1)
	s.LastUsed = shared.NowMS()
	if failure != nil && fault.Classify(failure).Kind == "busy" {
		// Only the single current recovery probe advances the backoff. Replies
		// from the same earlier batch may extend a deadline, never compound it.
		if shared.Truth(account["_busy_probe"]) && s.BusyGeneration == shared.Integer(account["_busy_generation"]) {
			s.BusyBackoffStep = min(7, s.BusyBackoffStep+1)
		}
		delay := accountBusyDelay(params, s.BusyBackoffStep, failure.RetryAfter)
		s.BusyGeneration++
		s.BusyUntil = max(s.BusyUntil, shared.NowMS()+max(50*time.Millisecond, delay).Milliseconds())
		s.BusyProbe = false
	} else if s.BusyGeneration == shared.Integer(account["_busy_generation"]) {
		s.BusyProbe = false
		if completed && failure == nil {
			s.BusyUntil, s.BusyBackoffStep = 0, 0
		}
	}
	a.notifyAvailableLocked()
}

func accountBusyDelay(params shared.M, step int, retryAfter time.Duration) time.Duration {
	delay := max(50*time.Millisecond, shared.Ms(shared.Fallback(params["rate_limit_retry_interval_ms"], 1000)))
	limit := max(5*time.Second, delay)
	for i := 0; i < step && delay < limit; i++ {
		delay = min(limit, delay*2)
	}
	var jitter time.Duration
	if shared.Truth(params["jitter"]) {
		jitter = time.Duration(rand.Float64() * float64(min(250*time.Millisecond, delay/5)))
	}
	return max(delay, retryAfter) + jitter
}
