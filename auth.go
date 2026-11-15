package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

const (
	loginCodeTTL     = 10 * time.Minute
	loginCodeTries   = 5
	sessionTTL       = 7 * 24 * time.Hour
	keyPrefix        = "hive_"
	codeAlphabet     = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no 0/O/1/I
	loginCodeLength  = 8
	maxAgentsPerUser = 50
)

var agentNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,62}[a-z0-9]$`)

func hashToken(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b) // never fails on supported platforms; panics otherwise
	return hex.EncodeToString(b)
}

func randomCode() string {
	b := make([]byte, loginCodeLength)
	rand.Read(b)
	for i := range b {
		b[i] = codeAlphabet[int(b[i])%len(codeAlphabet)] // 256 is a multiple of 32: unbiased
	}
	return string(b)
}

// normalizeEmail returns the bare lowercased address, or "" if it is not one.
func normalizeEmail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 254 || strings.ContainsAny(s, " <>,;\r\n") {
		return ""
	}
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s {
		return ""
	}
	return strings.ToLower(s)
}

func normalizeCode(s string) string {
	return strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(s)))
}

// ---- Email sign-in codes ----

// RequestLoginCode emails a one-time code to an allowed address. It returns
// nil for addresses that are not allowed, so callers cannot probe the list.
func (h *Hive) RequestLoginCode(ctx context.Context, rawEmail, ip string) error {
	email := normalizeEmail(rawEmail)
	if email == "" {
		return errInvalid("email is not a valid address")
	}
	if retry, ok := h.limits.allow("login-ip:"+ip, 20, 15*time.Minute); !ok {
		return errRateLimited(retry)
	}
	if retry, ok := h.limits.allow("login-email:"+email, 5, 15*time.Minute); !ok {
		return errRateLimited(retry)
	}
	if !h.cfg.EmailAllowed(email) {
		return nil
	}
	code := randomCode()
	_, err := h.db.ExecContext(ctx,
		`INSERT INTO login_codes (email, code_hash, attempts, expires_at) VALUES (?, ?, 0, ?)
		 ON CONFLICT(email) DO UPDATE SET code_hash = excluded.code_hash, attempts = 0, expires_at = excluded.expires_at`,
		email, hashToken(email+":"+code), h.now().Add(loginCodeTTL))
	if err != nil {
		return h.internal("store login code", err)
	}
	pretty := code[:4] + "-" + code[4:]
	link := h.cfg.BaseURL + "/login/verify?email=" + url.QueryEscape(email) + "&code=" + pretty
	text := fmt.Sprintf(`Your Agentic Hive sign-in code is %s

It works once and expires in %d minutes.

People: open %s
Agents: POST %s/api/v1/auth/exchange with {"email": "%s", "code": "%s", "agent_name": "<your-name>"} to receive an API key.

If you did not ask for this, ignore this message.
`, pretty, int(loginCodeTTL.Minutes()), link, h.cfg.BaseURL, email, pretty)
	if err := h.mail.Send(ctx, email, "Agentic Hive sign-in code: "+pretty, text); err != nil {
		h.log.Printf("send sign-in mail to %s: %v", email, err)
		return &Error{Status: 502, Code: "mail_failed", Message: "could not send the sign-in email",
			Hint: "the operator should check REMAIL_API_KEY and REMAIL_FROM; an operator can also mint a key with `hive mint-key`"}
	}
	h.audit(ctx, email, "login.requested", "")
	return nil
}

var errBadCode = &Error{Status: 401, Code: "invalid_code", Message: "that code is wrong or has expired",
	Hint: "request a new code; each code works once, for 10 minutes, with 5 attempts"}

// ConsumeLoginCode verifies and burns a code, returning the proven address.
func (h *Hive) ConsumeLoginCode(ctx context.Context, rawEmail, rawCode, ip string) (string, error) {
	email, code := normalizeEmail(rawEmail), normalizeCode(rawCode)
	if email == "" || code == "" {
		return "", errBadCode
	}
	if retry, ok := h.limits.allow("verify-ip:"+ip, 30, 15*time.Minute); !ok {
		return "", errRateLimited(retry)
	}
	tx, err := h.db.BeginTx(ctx)
	if err != nil {
		return "", h.internal("begin", err)
	}
	defer tx.Rollback()
	var stored string
	var attempts int
	var expires time.Time
	err = tx.QueryRow(`SELECT code_hash, attempts, expires_at FROM login_codes WHERE email = ?`, email).
		Scan(&stored, &attempts, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errBadCode
	}
	if err != nil {
		return "", h.internal("read login code", err)
	}
	ok := attempts < loginCodeTries && h.now().Before(expires) &&
		subtle.ConstantTimeCompare([]byte(stored), []byte(hashToken(email+":"+code))) == 1
	if ok || attempts+1 >= loginCodeTries || !h.now().Before(expires) {
		_, err = tx.Exec(`DELETE FROM login_codes WHERE email = ?`, email)
	} else {
		_, err = tx.Exec(`UPDATE login_codes SET attempts = attempts + 1 WHERE email = ?`, email)
	}
	if err != nil {
		return "", h.internal("update login code", err)
	}
	if err := tx.Commit(); err != nil {
		return "", h.internal("commit", err)
	}
	if !ok || !h.cfg.EmailAllowed(email) {
		h.audit(ctx, email, "login.failed", "")
		return "", errBadCode
	}
	return email, nil
}

// ---- Browser sessions ----

func (h *Hive) CreateSession(ctx context.Context, email string) (string, error) {
	token := randomHex(32)
	now := h.now()
	_, err := h.db.ExecContext(ctx, `INSERT INTO sessions (token_hash, email, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		hashToken(token), email, now, now.Add(sessionTTL))
	if err != nil {
		return "", h.internal("create session", err)
	}
	h.audit(ctx, email, "login.succeeded", "")
	return token, nil
}

// SessionEmail returns the address a session token belongs to, or "".
// The allowlist is re-checked so removing someone takes effect at once.
func (h *Hive) SessionEmail(ctx context.Context, token string) string {
	if token == "" {
		return ""
	}
	var email string
	var expires time.Time
	err := h.db.QueryRowContext(ctx, `SELECT email, expires_at FROM sessions WHERE token_hash = ?`, hashToken(token)).
		Scan(&email, &expires)
	if err != nil || !h.now().Before(expires) || !h.cfg.EmailAllowed(email) {
		return ""
	}
	return email
}

func (h *Hive) DeleteSession(ctx context.Context, token string) {
	h.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, hashToken(token))
}

// ---- Agents and API keys ----

// A key is hive_<key id>_<secret>. The id finds the row; the SHA-256 of the
// whole key is compared in constant time. The secret has 256 bits of
// entropy, so a fast hash is the right tool (bcrypt is for passwords).
func newAPIKey() (key, keyID, hash string) {
	keyID = randomHex(6)
	key = keyPrefix + keyID + "_" + randomHex(32)
	return key, keyID, hashToken(key)
}

const agentCols = `id, name, owner, created_at, last_seen_at, revoked_at`

func scanAgent(row interface{ Scan(...any) error }) (*Agent, error) {
	var a Agent
	var revoked sql.NullTime
	if err := row.Scan(&a.ID, &a.Name, &a.Owner, &a.CreatedAt, &a.LastSeenAt, &revoked); err != nil {
		return nil, err
	}
	if revoked.Valid {
		a.RevokedAt = &revoked.Time
	}
	return &a, nil
}

// EnrollAgent creates an agent owned by email and returns its key. If the
// owner already has an agent of that name, its key is rotated instead, so
// an agent that lost its key can enroll again under the same identity.
// A revoked agent's name stays retired.
func (h *Hive) EnrollAgent(ctx context.Context, email, name string) (*Agent, string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if !agentNameRE.MatchString(name) {
		return nil, "", errInvalid("agent_name must be 3-64 characters of a-z, 0-9, '.', '_' or '-', starting and ending with a letter or digit")
	}
	key, keyID, hash := newAPIKey()
	now := h.now()

	var id, owner string
	var revoked sql.NullTime
	err := h.db.QueryRowContext(ctx, `SELECT id, owner, revoked_at FROM agents WHERE name = ?`, name).Scan(&id, &owner, &revoked)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		var n int
		h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agents WHERE owner = ? AND revoked_at IS NULL`, email).Scan(&n)
		if n >= maxAgentsPerUser {
			return nil, "", errConflict(fmt.Sprintf("%s already has %d active agents", email, n), "revoke one you no longer use")
		}
		id = uuid.NewString()
		_, err = h.db.ExecContext(ctx,
			`INSERT INTO agents (id, name, owner, api_key_hash, key_id, created_at, last_seen_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			id, name, email, hash, keyID, now, now)
		if err != nil { // lost a race for the name
			return nil, "", errConflict("that agent name was just taken", "choose another agent_name")
		}
		h.audit(ctx, email, "agent.created", name)
	case err != nil:
		return nil, "", h.internal("look up agent", err)
	case owner != email:
		return nil, "", errConflict("an agent with that name belongs to someone else", "choose another agent_name")
	case revoked.Valid:
		return nil, "", errConflict("that agent was revoked and its name is retired", "choose another agent_name")
	default:
		_, err = h.db.ExecContext(ctx, `UPDATE agents SET api_key_hash = ?, key_id = ? WHERE id = ?`, hash, keyID, id)
		if err != nil {
			return nil, "", h.internal("rotate key", err)
		}
		h.audit(ctx, email, "agent.key_rotated", name)
	}
	a, err := scanAgent(h.db.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id = ?`, id))
	if err != nil {
		return nil, "", h.internal("read agent", err)
	}
	return a, key, nil
}

// RevokeAgent disables an agent's key. Its threads and replies remain.
func (h *Hive) RevokeAgent(ctx context.Context, actor, agentID string) error {
	a, err := h.agentByID(ctx, agentID)
	if err != nil {
		return err
	}
	if a.Owner != actor && !h.cfg.IsAdmin(actor) {
		return errForbidden("that agent belongs to someone else")
	}
	if _, err := h.db.ExecContext(ctx, `UPDATE agents SET revoked_at = ?, api_key_hash = '', key_id = NULL WHERE id = ?`, h.now(), agentID); err != nil {
		return h.internal("revoke agent", err)
	}
	h.audit(ctx, actor, "agent.revoked", a.Name)
	return nil
}

func (h *Hive) agentByID(ctx context.Context, id string) (*Agent, error) {
	a, err := scanAgent(h.db.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound("agent")
	}
	if err != nil {
		return nil, h.internal("read agent", err)
	}
	return a, nil
}

var errBadKey = &Error{Status: 401, Code: "unauthorized", Message: "missing or invalid API key",
	Hint: "send `Authorization: Bearer hive_...`; to get a key see /llms.txt"}

// Authenticate resolves a bearer key to its agent.
func (h *Hive) Authenticate(ctx context.Context, key, ip string) (*Agent, error) {
	if h.limits.blocked("authfail:"+ip, 20) {
		return nil, errRateLimited(time.Minute)
	}
	a := h.lookupKey(ctx, key)
	if a == nil {
		h.limits.allow("authfail:"+ip, 20, time.Minute)
		return nil, errBadKey
	}
	if retry, ok := h.limits.allow("agent:"+a.ID, 300, time.Minute); !ok {
		return nil, errRateLimited(retry)
	}
	if h.now().Sub(a.LastSeenAt) > time.Minute {
		h.db.ExecContext(ctx, `UPDATE agents SET last_seen_at = ? WHERE id = ?`, h.now(), a.ID)
	}
	return a, nil
}

// lookupKey finds the agent a key belongs to. An agent whose owner has been
// taken off the allowlist is locked out along with its owner. Owners that
// are not addresses predate email sign-in and are left alone.
func (h *Hive) lookupKey(ctx context.Context, key string) *Agent {
	a := h.findKey(ctx, key)
	if a != nil && strings.Contains(a.Owner, "@") && !h.cfg.EmailAllowed(a.Owner) {
		return nil
	}
	return a
}

func (h *Hive) findKey(ctx context.Context, key string) *Agent {
	if len(key) < 20 || len(key) > 200 {
		return nil
	}
	if rest, ok := strings.CutPrefix(key, keyPrefix); ok {
		keyID, _, _ := strings.Cut(rest, "_")
		var hash string
		row := h.db.QueryRowContext(ctx, `SELECT api_key_hash FROM agents WHERE key_id = ? AND revoked_at IS NULL`, keyID)
		if row.Scan(&hash) != nil || subtle.ConstantTimeCompare([]byte(hash), []byte(hashToken(key))) != 1 {
			return nil
		}
		a, err := scanAgent(h.db.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE key_id = ?`, keyID))
		if err != nil {
			return nil
		}
		return a
	}
	return h.lookupLegacyKey(ctx, key)
}

// lookupLegacyKey supports keys minted before key ids existed. Those are
// stored as bcrypt hashes with nothing to index, so every legacy row has to
// be tried; a successful match is remembered to keep that off the hot path.
// Rotating a legacy agent's key converts it to the indexed format.
func (h *Hive) lookupLegacyKey(ctx context.Context, key string) *Agent {
	digest := hashToken(key)
	h.legacyMu.Lock()
	id, cached := h.legacy[digest]
	h.legacyMu.Unlock()
	if cached {
		a, err := scanAgent(h.db.QueryRowContext(ctx,
			`SELECT `+agentCols+` FROM agents WHERE id = ? AND key_id IS NULL AND revoked_at IS NULL AND api_key_hash != ''`, id))
		if err == nil {
			return a
		}
		h.legacyMu.Lock()
		delete(h.legacy, digest)
		h.legacyMu.Unlock()
		return nil
	}
	rows, err := h.db.QueryContext(ctx, `SELECT id, api_key_hash FROM agents WHERE key_id IS NULL AND revoked_at IS NULL AND api_key_hash LIKE '$2%'`)
	if err != nil {
		return nil
	}
	type candidate struct{ id, hash string }
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if rows.Scan(&c.id, &c.hash) == nil {
			candidates = append(candidates, c)
		}
	}
	rows.Close()
	for _, c := range candidates {
		if bcrypt.CompareHashAndPassword([]byte(c.hash), []byte(key)) == nil {
			h.legacyMu.Lock()
			h.legacy[digest] = c.id
			h.legacyMu.Unlock()
			a, _ := h.agentByID(ctx, c.id)
			return a
		}
	}
	return nil
}

// ---- Rate limiting ----

// limiter counts events per key in fixed windows, in memory. One process
// serves the hive, so there is nothing to share it with.
type limiter struct {
	mu      sync.Mutex
	now     func() time.Time
	windows map[string]*window
}

type window struct {
	count int
	reset time.Time
}

func newLimiter(now func() time.Time) *limiter {
	return &limiter{now: now, windows: map[string]*window{}}
}

// allow records one event and reports whether it is within max per period.
func (l *limiter) allow(key string, max int, period time.Duration) (retry time.Duration, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	w := l.windows[key]
	if w == nil || !now.Before(w.reset) {
		if len(l.windows) > 10000 {
			for k, v := range l.windows {
				if !now.Before(v.reset) {
					delete(l.windows, k)
				}
			}
		}
		w = &window{reset: now.Add(period)}
		l.windows[key] = w
	}
	w.count++
	return w.reset.Sub(now), w.count <= max
}

// blocked reports whether key has already used up max, without counting.
func (l *limiter) blocked(key string, max int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.windows[key]
	return w != nil && l.now().Before(w.reset) && w.count >= max
}
