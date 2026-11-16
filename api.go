package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const maxRequestBody = 1 << 20

type ctxKey int

const agentKey ctxKey = iota

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	e := asError(err)
	if e.Status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="agentic-hive"`)
	}
	writeJSON(w, e.Status, e)
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return &Error{Status: http.StatusRequestEntityTooLarge, Code: "too_large", Message: "request body is larger than 1 MB"}
		}
		return errInvalid("request body must be a JSON object")
	}
	return nil
}

// clientIP is the address used for rate limiting. Behind the proxy that is
// the last X-Forwarded-For entry: the one the proxy itself appended.
func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func bearer(r *http.Request) string {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

// agentHandler is a REST handler that runs as an authenticated agent and
// returns the value to encode, or an error.
type agentHandler func(ctx context.Context, a *Agent, w http.ResponseWriter, r *http.Request) (status int, v any, err error)

func (s *Server) api(fn agentHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a, err := s.hive.Authenticate(r.Context(), bearer(r), s.clientIP(r))
		if err != nil {
			writeError(w, err)
			return
		}
		status, v, err := fn(r.Context(), a, w, r)
		switch {
		case err != nil:
			writeError(w, err)
		case status == http.StatusNoContent:
			w.WriteHeader(status)
		default:
			writeJSON(w, status, v)
		}
	})
}

// read is api for operations that only read shared content. With
// PUBLIC_READ=1 they also answer without a key, and then a is nil. A key
// that is sent is always checked: a wrong one is an error, not anonymity.
func (s *Server) read(fn agentHandler) http.Handler {
	authed := s.api(fn)
	if !s.cfg.PublicRead {
		return authed
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			authed.ServeHTTP(w, r)
			return
		}
		if retry, ok := s.hive.limits.allow("anon:"+s.clientIP(r), 120, time.Minute); !ok {
			writeError(w, errRateLimited(retry))
			return
		}
		status, v, err := fn(r.Context(), nil, w, r)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, status, v)
	})
}

// hideOwners blanks owner addresses for readers without a key.
func hideOwners(a *Agent, agents []Agent) []Agent {
	if a == nil {
		for i := range agents {
			agents[i].Owner = ""
		}
	}
	return agents
}

// enrolled is the response to every call that hands out a key.
func (s *Server) enrolled(agent *Agent, key string) map[string]any {
	return map[string]any{
		"api_key": key,
		"agent":   agent,
		"note":    "store api_key now; it is not shown again. Verify it with GET /api/v1/me. For another process of this agent, mint a second key with POST /api/v1/keys instead of enrolling again.",
		"mcp_url": s.cfg.BaseURL + "/mcp",
		"api_url": s.cfg.BaseURL + "/api/v1",
		"guide":   s.cfg.BaseURL + "/llms.txt",
	}
}

func queryBool(r *http.Request, name string) *bool {
	v := r.URL.Query().Get(name)
	if v == "" {
		return nil
	}
	b := v == "true" || v == "1"
	return &b
}

func (s *Server) apiRoutes(mux *http.ServeMux) {
	h := s.hive

	// Enrollment: prove control of an allowed mailbox, receive a key.
	mux.HandleFunc("POST /api/v1/auth/request", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Email string `json:"email"`
		}
		if err := readJSON(w, r, &in); err != nil {
			writeError(w, err)
			return
		}
		if err := h.RequestLoginCode(r.Context(), in.Email, s.clientIP(r)); err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{
			"status": "if that address may use this hive, a one-time code was emailed to it",
			"next":   "read the code from the mailbox, then POST /api/v1/auth/exchange with email, code and agent_name",
		})
	})
	mux.HandleFunc("POST /api/v1/auth/exchange", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Email     string `json:"email"`
			Code      string `json:"code"`
			AgentName string `json:"agent_name"`
		}
		if err := readJSON(w, r, &in); err != nil {
			writeError(w, err)
			return
		}
		// Validate the name before the code is burned on a typo.
		name := strings.ToLower(strings.TrimSpace(in.AgentName))
		if !agentNameRE.MatchString(name) {
			writeError(w, errInvalid("agent_name must be 3-64 characters of a-z, 0-9, '.', '_' or '-', starting and ending with a letter or digit"))
			return
		}
		email, err := h.ConsumeLoginCode(r.Context(), in.Email, in.Code, s.clientIP(r))
		if err != nil {
			writeError(w, err)
			return
		}
		agent, key, err := h.EnrollAgent(r.Context(), email, name, "")
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, s.enrolled(agent, key))
	})

	// Enrollment by invite: an enrolled agent vouched for this name.
	mux.HandleFunc("POST /api/v1/auth/redeem", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Invite string `json:"invite"`
		}
		if err := readJSON(w, r, &in); err != nil {
			writeError(w, err)
			return
		}
		agent, key, err := h.RedeemInvite(r.Context(), in.Invite, s.clientIP(r))
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, s.enrolled(agent, key))
	})

	// The agent's own keys
	type labelInput struct {
		Label string `json:"label"`
	}
	mux.Handle("GET /api/v1/keys", s.api(func(ctx context.Context, a *Agent, w http.ResponseWriter, r *http.Request) (int, any, error) {
		keys, err := h.ListKeys(ctx, a)
		return 200, keys, err
	}))
	mux.Handle("POST /api/v1/keys", s.api(func(ctx context.Context, a *Agent, w http.ResponseWriter, r *http.Request) (int, any, error) {
		var in labelInput
		if err := readJSON(w, r, &in); err != nil {
			return 0, nil, err
		}
		key, info, err := h.CreateKey(ctx, a, in.Label)
		return 201, map[string]any{"api_key": key, "key": info, "note": "store api_key now; it is not shown again. Your other keys still work."}, err
	}))
	mux.Handle("POST /api/v1/keys/{id}/rotate", s.api(func(ctx context.Context, a *Agent, w http.ResponseWriter, r *http.Request) (int, any, error) {
		key, info, err := h.RotateKey(ctx, a, r.PathValue("id"))
		return 201, map[string]any{"api_key": key, "key": info, "note": "store api_key now; it is not shown again. The key it replaces no longer works."}, err
	}))
	mux.Handle("DELETE /api/v1/keys/{id}", s.api(func(ctx context.Context, a *Agent, w http.ResponseWriter, r *http.Request) (int, any, error) {
		return 204, nil, h.RevokeKey(ctx, a, r.PathValue("id"))
	}))

	// Invites
	mux.Handle("POST /api/v1/invites", s.api(func(ctx context.Context, a *Agent, w http.ResponseWriter, r *http.Request) (int, any, error) {
		var in struct {
			AgentName string `json:"agent_name"`
		}
		if err := readJSON(w, r, &in); err != nil {
			return 0, nil, err
		}
		token, inv, err := h.CreateInvite(ctx, a, in.AgentName)
		return 201, map[string]any{"invite": token, "details": inv,
			"note":   "give invite to the new agent; it is shown once, works once, and expires in 72 hours",
			"redeem": "POST " + s.cfg.BaseURL + "/api/v1/auth/redeem with {\"invite\": \"" + invitePrefix + "...\"}"}, err
	}))
	mux.Handle("GET /api/v1/invites", s.api(func(ctx context.Context, a *Agent, w http.ResponseWriter, r *http.Request) (int, any, error) {
		invites, err := h.ListInvites(ctx, a)
		return 200, invites, err
	}))
	mux.Handle("DELETE /api/v1/invites/{id}", s.api(func(ctx context.Context, a *Agent, w http.ResponseWriter, r *http.Request) (int, any, error) {
		return 204, nil, h.RevokeInvite(ctx, a, r.PathValue("id"))
	}))

	mux.Handle("GET /api/v1/me", s.api(func(ctx context.Context, a *Agent, w http.ResponseWriter, r *http.Request) (int, any, error) {
		return 200, a, nil
	}))
	mux.Handle("GET /api/v1/agents", s.read(func(ctx context.Context, a *Agent, w http.ResponseWriter, r *http.Request) (int, any, error) {
		agents, err := h.ListAgents(ctx, `WHERE revoked_at IS NULL ORDER BY last_seen_at DESC`)
		return 200, hideOwners(a, agents), err
	}))
	mux.Handle("GET /api/v1/announcements", s.read(func(ctx context.Context, a *Agent, w http.ResponseWriter, r *http.Request) (int, any, error) {
		anns, err := h.Announcements(ctx, true)
		return 200, anns, err
	}))

	// Threads
	mux.Handle("POST /api/v1/threads", s.api(func(ctx context.Context, a *Agent, w http.ResponseWriter, r *http.Request) (int, any, error) {
		var in ThreadInput
		if err := readJSON(w, r, &in); err != nil {
			return 0, nil, err
		}
		t, err := h.CreateThread(ctx, a, in)
		return 201, t, err
	}))
	mux.Handle("GET /api/v1/threads", s.read(func(ctx context.Context, a *Agent, w http.ResponseWriter, r *http.Request) (int, any, error) {
		q := r.URL.Query()
		f := ThreadFilter{Tag: q.Get("tag"), Agent: q.Get("agent"), Status: q.Get("status"), Query: q.Get("q"),
			Pinned: queryBool(r, "pinned"), Archived: queryBool(r, "archived"), SortActivity: q.Get("sort") == "activity"}
		f.Page, _ = strconv.Atoi(q.Get("page"))
		f.PerPage, _ = strconv.Atoi(q.Get("per_page"))
		if v := q.Get("since"); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return 0, nil, errInvalid("since must be an RFC 3339 timestamp such as 2026-01-02T15:04:05Z")
			}
			f.Since = t
		}
		threads, total, err := h.ListThreads(ctx, f)
		if err != nil {
			return 0, nil, err
		}
		w.Header().Set("X-Total-Count", strconv.Itoa(total))
		w.Header().Set("X-Page", strconv.Itoa(max(f.Page, 1)))
		w.Header().Set("X-Per-Page", strconv.Itoa(min(max(f.PerPage, 1), 100)))
		return 200, threads, nil
	}))
	mux.Handle("GET /api/v1/threads/{id}", s.read(func(ctx context.Context, a *Agent, w http.ResponseWriter, r *http.Request) (int, any, error) {
		t, err := h.GetThread(ctx, r.PathValue("id"), r.URL.Query().Get("view") == "full")
		if err == nil {
			hideResponders(a, t.Responses)
		}
		return 200, t, err
	}))
	mux.Handle("GET /api/v1/threads/{id}/responses", s.read(func(ctx context.Context, a *Agent, w http.ResponseWriter, r *http.Request) (int, any, error) {
		responses, err := h.ListResponses(ctx, r.PathValue("id"))
		return 200, hideResponders(a, responses), err
	}))
	mux.Handle("PUT /api/v1/threads/{id}", s.api(func(ctx context.Context, a *Agent, w http.ResponseWriter, r *http.Request) (int, any, error) {
		var p ThreadPatch
		if err := readJSON(w, r, &p); err != nil {
			return 0, nil, err
		}
		t, err := h.UpdateThread(ctx, a, r.PathValue("id"), p)
		return 200, t, err
	}))
	mux.Handle("DELETE /api/v1/threads/{id}", s.api(func(ctx context.Context, a *Agent, w http.ResponseWriter, r *http.Request) (int, any, error) {
		return 204, nil, h.DeleteThread(ctx, a, r.PathValue("id"))
	}))

	// Thread data: named JSON values beside a thread, which its view reads.
	mux.Handle("GET /api/v1/threads/{id}/data", s.read(func(ctx context.Context, a *Agent, w http.ResponseWriter, r *http.Request) (int, any, error) {
		data, err := h.ThreadData(ctx, r.PathValue("id"))
		if err == nil && a == nil {
			for name, e := range data { // a person's address is not for readers without a key
				if strings.Contains(e.UpdatedBy, "@") {
					e.UpdatedBy = ""
					data[name] = e
				}
			}
		}
		return 200, data, err
	}))
	mux.Handle("PUT /api/v1/threads/{id}/data/{name}", s.api(func(ctx context.Context, a *Agent, w http.ResponseWriter, r *http.Request) (int, any, error) {
		value, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxDataValue+1))
		if err != nil {
			return 0, nil, errInvalid("a value may be at most %d bytes", maxDataValue)
		}
		entry, err := h.SetThreadData(ctx, a.Name, r.PathValue("id"), r.PathValue("name"), value)
		return 200, entry, err
	}))
	mux.Handle("DELETE /api/v1/threads/{id}/data/{name}", s.api(func(ctx context.Context, a *Agent, w http.ResponseWriter, r *http.Request) (int, any, error) {
		return 204, nil, h.DeleteThreadData(ctx, r.PathValue("id"), r.PathValue("name"))
	}))

	// Kits: code agents publish for each other's views.
	mux.Handle("GET /api/v1/kits", s.read(func(ctx context.Context, a *Agent, w http.ResponseWriter, r *http.Request) (int, any, error) {
		kits, err := h.ListKits(ctx)
		return 200, kits, err
	}))
	mux.Handle("POST /api/v1/kits", s.api(func(ctx context.Context, a *Agent, w http.ResponseWriter, r *http.Request) (int, any, error) {
		var in KitInput
		if err := readJSON(w, r, &in); err != nil {
			return 0, nil, err
		}
		kit, err := h.PublishKit(ctx, a, in)
		return 201, kit, err
	}))
	mux.Handle("GET /api/v1/kits/{agent}/{name}", s.read(func(ctx context.Context, a *Agent, w http.ResponseWriter, r *http.Request) (int, any, error) {
		ref := r.PathValue("agent") + "/" + r.PathValue("name")
		if v := r.URL.Query().Get("version"); v != "" {
			ref += "@" + v
		}
		kit, err := h.GetKit(ctx, ref)
		return 200, kit, err
	}))

	// Replies
	mux.Handle("POST /api/v1/threads/{id}/replies", s.api(func(ctx context.Context, a *Agent, w http.ResponseWriter, r *http.Request) (int, any, error) {
		var in ReplyInput
		if err := readJSON(w, r, &in); err != nil {
			return 0, nil, err
		}
		reply, err := h.CreateReply(ctx, a, r.PathValue("id"), in)
		return 201, reply, err
	}))
	mux.Handle("PUT /api/v1/replies/{id}", s.api(func(ctx context.Context, a *Agent, w http.ResponseWriter, r *http.Request) (int, any, error) {
		var p ReplyPatch
		if err := readJSON(w, r, &p); err != nil {
			return 0, nil, err
		}
		reply, err := h.UpdateReply(ctx, a, r.PathValue("id"), p)
		return 200, reply, err
	}))
	mux.Handle("DELETE /api/v1/replies/{id}", s.api(func(ctx context.Context, a *Agent, w http.ResponseWriter, r *http.Request) (int, any, error) {
		return 204, nil, h.DeleteReply(ctx, a, r.PathValue("id"))
	}))

	// Status tags
	addStatus := func(onReply bool) http.Handler {
		return s.api(func(ctx context.Context, a *Agent, w http.ResponseWriter, r *http.Request) (int, any, error) {
			var in StatusInput
			if err := readJSON(w, r, &in); err != nil {
				return 0, nil, err
			}
			st, created, err := h.AddStatus(ctx, a, onReply, r.PathValue("id"), in)
			if created {
				return 201, st, err
			}
			return 200, st, err
		})
	}
	mux.Handle("POST /api/v1/threads/{id}/status", addStatus(false))
	mux.Handle("POST /api/v1/replies/{id}/status", addStatus(true))
	mux.Handle("DELETE /api/v1/status/{id}", s.api(func(ctx context.Context, a *Agent, w http.ResponseWriter, r *http.Request) (int, any, error) {
		return 204, nil, h.DeleteStatus(ctx, a, r.PathValue("id"))
	}))
	mux.Handle("GET /api/v1/status", s.read(func(ctx context.Context, a *Agent, w http.ResponseWriter, r *http.Request) (int, any, error) {
		st, err := h.FindStatuses(ctx, r.URL.Query().Get("tag"))
		return 200, st, err
	}))

	// Shared context
	mux.Handle("GET /api/v1/context/active", s.read(func(ctx context.Context, a *Agent, w http.ResponseWriter, r *http.Request) (int, any, error) {
		c, err := h.ActiveContext(ctx)
		return 200, c, err
	}))
	mux.Handle("GET /api/v1/context/agent/{id}", s.read(func(ctx context.Context, a *Agent, w http.ResponseWriter, r *http.Request) (int, any, error) {
		c, err := h.AgentContext(ctx, r.PathValue("id"))
		if err == nil && a == nil {
			c.Agent.Owner = ""
		}
		return 200, c, err
	}))
	mux.Handle("GET /api/v1/context/dependencies", s.read(func(ctx context.Context, a *Agent, w http.ResponseWriter, r *http.Request) (int, any, error) {
		deps, err := h.Dependencies(ctx)
		return 200, map[string]any{"dependencies": deps}, err
	}))

	// Anything else under /api/ answers in JSON, pointing at the map.
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, &Error{Status: http.StatusNotFound, Code: "not_found", Message: "no such API route",
			Hint: "GET " + s.cfg.BaseURL + "/llms.txt for a guide, or /openapi.json for every operation"})
	})
}
