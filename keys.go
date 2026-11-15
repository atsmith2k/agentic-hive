package main

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	maxKeysPerAgent = 10
	maxKeyLabelLen  = 64
	inviteTTL       = 72 * time.Hour
	invitePrefix    = "hiveinv_"
	maxOpenInvites  = 20
)

// ---- An agent's own keys ----

// mintAgentKey stores a new key for an agent and returns it. This is the
// only place a key's secret exists outside the caller's hands.
func (h *Hive) mintAgentKey(ctx context.Context, agentID, label string) (string, *APIKey, error) {
	key, keyID, hash := newAPIKey()
	now := h.now()
	if _, err := h.db.ExecContext(ctx, `INSERT INTO agent_keys (id, agent_id, key_hash, label, created_at) VALUES (?, ?, ?, ?, ?)`,
		keyID, agentID, hash, label, now); err != nil {
		return "", nil, h.internal("store key", err)
	}
	return key, &APIKey{ID: keyID, Label: label, CreatedAt: now}, nil
}

// revokeAllKeys ends every key an agent has, including a pre-indexing one.
func (h *Hive) revokeAllKeys(ctx context.Context, agentID string) error {
	if _, err := h.db.ExecContext(ctx, `UPDATE agent_keys SET revoked_at = ? WHERE agent_id = ? AND revoked_at IS NULL`, h.now(), agentID); err != nil {
		return h.internal("revoke keys", err)
	}
	if _, err := h.db.ExecContext(ctx, `UPDATE agents SET api_key_hash = '' WHERE id = ?`, agentID); err != nil {
		return h.internal("revoke legacy key", err)
	}
	return nil
}

func cleanLabel(label string) (string, *Error) {
	label = strings.TrimSpace(label)
	if label == "" {
		return "", errInvalid("label is required: say what will use this key, for example \"ci\" or \"laptop\"")
	}
	if len(label) > maxKeyLabelLen {
		return "", errInvalid("label is longer than %d bytes", maxKeyLabelLen)
	}
	return label, nil
}

// ListKeys returns the agent's active keys, oldest first.
func (h *Hive) ListKeys(ctx context.Context, a *Agent) ([]APIKey, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT id, label, created_at, last_used_at FROM agent_keys WHERE agent_id = ? AND revoked_at IS NULL ORDER BY created_at, id`, a.ID)
	if err != nil {
		return nil, h.internal("query keys", err)
	}
	defer rows.Close()
	keys := []APIKey{}
	for rows.Next() {
		var k APIKey
		var used sql.NullTime
		if err := rows.Scan(&k.ID, &k.Label, &k.CreatedAt, &used); err != nil {
			return nil, h.internal("scan key", err)
		}
		if used.Valid {
			k.LastUsedAt = &used.Time
		}
		k.Current = k.ID == a.keyID
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// CreateKey mints an additional key, leaving the others valid, so a second
// process of the same agent does not have to re-enroll and lock out the first.
func (h *Hive) CreateKey(ctx context.Context, a *Agent, rawLabel string) (string, *APIKey, error) {
	label, e := cleanLabel(rawLabel)
	if e != nil {
		return "", nil, e
	}
	keys, err := h.ListKeys(ctx, a)
	if err != nil {
		return "", nil, err
	}
	if len(keys) >= maxKeysPerAgent {
		return "", nil, errConflict("this agent already has the maximum of 10 active keys", "revoke one with DELETE /api/v1/keys/{id}")
	}
	key, info, err := h.mintAgentKey(ctx, a.ID, label)
	if err != nil {
		return "", nil, err
	}
	h.audit(ctx, a.Name, "key.created", label)
	return key, info, nil
}

func (h *Hive) ownKey(ctx context.Context, a *Agent, keyID string) (*APIKey, error) {
	keys, err := h.ListKeys(ctx, a)
	if err != nil {
		return nil, err
	}
	for i := range keys {
		if keys[i].ID == keyID {
			return &keys[i], nil
		}
	}
	return nil, errNotFound("key")
}

// RevokeKey ends one of the agent's keys. The last key cannot be revoked:
// that would lock the agent out, and rotating it does what is wanted.
func (h *Hive) RevokeKey(ctx context.Context, a *Agent, keyID string) error {
	k, err := h.ownKey(ctx, a, keyID)
	if err != nil {
		return err
	}
	var active int
	h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_keys WHERE agent_id = ? AND revoked_at IS NULL`, a.ID).Scan(&active)
	if active <= 1 {
		return errConflict("this is the agent's only key", "rotate it with POST /api/v1/keys/{id}/rotate, which replaces it in one step")
	}
	if _, err := h.db.ExecContext(ctx, `UPDATE agent_keys SET revoked_at = ? WHERE id = ?`, h.now(), k.ID); err != nil {
		return h.internal("revoke key", err)
	}
	h.audit(ctx, a.Name, "key.revoked", k.Label)
	return nil
}

// RotateKey replaces one key with a new one carrying the same label. The
// old key stops working as soon as this returns.
func (h *Hive) RotateKey(ctx context.Context, a *Agent, keyID string) (string, *APIKey, error) {
	k, err := h.ownKey(ctx, a, keyID)
	if err != nil {
		return "", nil, err
	}
	key, info, err := h.mintAgentKey(ctx, a.ID, k.Label)
	if err != nil {
		return "", nil, err
	}
	if _, err := h.db.ExecContext(ctx, `UPDATE agent_keys SET revoked_at = ? WHERE id = ?`, h.now(), k.ID); err != nil {
		return "", nil, h.internal("revoke rotated key", err)
	}
	h.audit(ctx, a.Name, "key.rotated", k.Label)
	return key, info, nil
}

// ---- Invites: an enrolled agent vouches for a new one ----

var errInvitesOff = &Error{Status: 403, Code: "invites_disabled", Message: "agent invites are turned off on this hive",
	Hint: "enroll with a mailbox instead: POST /api/v1/auth/request"}

// CreateInvite returns a single-use token that lets the named agent enroll
// with no mailbox and no person. The new agent belongs to the inviter's
// owner, so it is accountable to the same mailbox, counts against the same
// limits, and loses access if that owner does. Inviting the name of an
// existing agent of the same owner is how that agent recovers a lost key.
func (h *Hive) CreateInvite(ctx context.Context, a *Agent, rawName string) (string, *Invite, error) {
	if !h.cfg.AgentInvites {
		return "", nil, errInvitesOff
	}
	name, e := cleanAgentName(rawName)
	if e != nil {
		return "", nil, e
	}
	if retry, ok := h.limits.allow("invite:"+a.ID, 30, time.Hour); !ok {
		return "", nil, errRateLimited(retry)
	}
	var owner string
	var revoked sql.NullTime
	err := h.db.QueryRowContext(ctx, `SELECT owner, revoked_at FROM agents WHERE name = ?`, name).Scan(&owner, &revoked)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return "", nil, h.internal("look up agent", err)
	case owner != a.Owner:
		return "", nil, errConflict("an agent with that name belongs to someone else", "choose another agent_name")
	case revoked.Valid:
		return "", nil, errConflict("that agent was revoked and its name is retired", "choose another agent_name")
	}
	now := h.now()
	var open int
	h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM invites WHERE invited_by = ? AND used_at IS NULL AND expires_at > ?`, a.ID, now).Scan(&open)
	if open >= maxOpenInvites {
		return "", nil, errConflict("this agent has 20 unused invites outstanding", "revoke some with DELETE /api/v1/invites/{id}")
	}
	token := invitePrefix + randomHex(32)
	inv := &Invite{ID: uuid.NewString(), AgentName: name, CreatedAt: now, ExpiresAt: now.Add(inviteTTL)}
	if _, err := h.db.ExecContext(ctx,
		`INSERT INTO invites (id, token_hash, agent_name, owner, invited_by, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		inv.ID, hashToken(token), name, a.Owner, a.ID, inv.CreatedAt, inv.ExpiresAt); err != nil {
		return "", nil, h.internal("store invite", err)
	}
	h.audit(ctx, a.Name, "invite.created", name)
	return token, inv, nil
}

// ListInvites returns the invites this agent issued, newest first.
func (h *Hive) ListInvites(ctx context.Context, a *Agent) ([]Invite, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT id, agent_name, created_at, expires_at, used_at FROM invites WHERE invited_by = ? ORDER BY created_at DESC LIMIT 100`, a.ID)
	if err != nil {
		return nil, h.internal("query invites", err)
	}
	defer rows.Close()
	out := []Invite{}
	for rows.Next() {
		var inv Invite
		var used sql.NullTime
		if err := rows.Scan(&inv.ID, &inv.AgentName, &inv.CreatedAt, &inv.ExpiresAt, &used); err != nil {
			return nil, h.internal("scan invite", err)
		}
		if used.Valid {
			inv.UsedAt = &used.Time
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// RevokeInvite cancels an unused invite this agent issued.
func (h *Hive) RevokeInvite(ctx context.Context, a *Agent, id string) error {
	res, err := h.db.ExecContext(ctx, `DELETE FROM invites WHERE id = ? AND invited_by = ? AND used_at IS NULL`, id, a.ID)
	if err != nil {
		return h.internal("revoke invite", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNotFound("unused invite")
	}
	return nil
}

var errBadInvite = &Error{Status: 401, Code: "invalid_invite", Message: "that invite is wrong, used or expired",
	Hint: "ask an enrolled agent for a new one (POST /api/v1/invites); each works once, for 72 hours"}

// RedeemInvite spends an invite and returns the invited agent with its key.
func (h *Hive) RedeemInvite(ctx context.Context, token, ip string) (*Agent, string, error) {
	if !h.cfg.AgentInvites {
		return nil, "", errInvitesOff
	}
	if retry, ok := h.limits.allow("redeem-ip:"+ip, 30, 15*time.Minute); !ok {
		return nil, "", errRateLimited(retry)
	}
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, invitePrefix) {
		return nil, "", errBadInvite
	}
	now := h.now()
	// Claiming the row is the single-use guarantee: only one request can
	// turn used_at from NULL.
	res, err := h.db.ExecContext(ctx, `UPDATE invites SET used_at = ? WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?`,
		now, hashToken(token), now)
	if err != nil {
		return nil, "", h.internal("claim invite", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, "", errBadInvite
	}
	var name, owner, inviter string
	if err := h.db.QueryRowContext(ctx, `SELECT agent_name, owner, invited_by FROM invites WHERE token_hash = ?`, hashToken(token)).
		Scan(&name, &owner, &inviter); err != nil {
		return nil, "", h.internal("read invite", err)
	}
	// The inviter must still be in good standing when the invite is used.
	var inviterRevoked sql.NullTime
	err = h.db.QueryRowContext(ctx, `SELECT revoked_at FROM agents WHERE id = ?`, inviter).Scan(&inviterRevoked)
	if err != nil || inviterRevoked.Valid || (strings.Contains(owner, "@") && !h.cfg.EmailAllowed(owner)) {
		return nil, "", errBadInvite
	}
	agent, key, err := h.EnrollAgent(ctx, owner, name, inviter)
	if err != nil {
		return nil, "", err
	}
	h.audit(ctx, name, "invite.redeemed", "")
	return agent, key, nil
}
