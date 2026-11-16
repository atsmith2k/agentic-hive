package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// fakeMailer records what would have been emailed.
type fakeMailer struct {
	mu   sync.Mutex
	sent map[string]string // address -> last message text
	fail bool
}

func (m *fakeMailer) Send(_ context.Context, to, subject, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return io.ErrUnexpectedEOF
	}
	m.sent[to] = text
	return nil
}

var codeRE = regexp.MustCompile(`sign-in code is ([A-Z0-9]{4}-[A-Z0-9]{4})`)

func (m *fakeMailer) code(t *testing.T, to string) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	match := codeRE.FindStringSubmatch(m.sent[to])
	if match == nil {
		t.Fatalf("no sign-in code was mailed to %s", to)
	}
	return match[1]
}

type testEnv struct {
	t    *testing.T
	hive *Hive
	mail *fakeMailer
	srv  *httptest.Server
}

// openTestStore opens an empty store of the named kind. Postgres tests need
// TEST_DATABASE_URL (CI provides one) and each gets its own schema.
func openTestStore(t *testing.T, store string) *DB {
	t.Helper()
	if store == "sqlite" {
		db, err := OpenDB(Config{DBPath: filepath.Join(t.TempDir(), "hive.db")})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	db, err := OpenDB(Config{DatabaseURL: testPostgresURL(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// testPostgresURL returns a connection string for a fresh, empty schema
// that is dropped when the test ends.
func testPostgresURL(t *testing.T) string {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		if os.Getenv("REQUIRE_POSTGRES_TESTS") == "1" { // CI: a skip there would hide a gap
			t.Fatal("REQUIRE_POSTGRES_TESTS=1 but TEST_DATABASE_URL is not set")
		}
		t.Skip("TEST_DATABASE_URL is not set; skipping Postgres")
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	schema := "t_" + randomHex(8)
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatalf("TEST_DATABASE_URL is set but unusable: %v", err)
	}
	t.Cleanup(func() {
		admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		admin.Close()
	})
	u, _ := url.Parse(base)
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	t.Setenv("TEST_STORE_URL", u.String())
	return u.String()
}

func newEnv(t *testing.T, store string) *testEnv {
	t.Helper()
	cfg := Config{BaseURL: "https://hive.test", AllowedDomains: []string{"johnnycode.ai"},
		AdminEmails: []string{"owner@johnnycode.ai"}, FrameAncestors: "'none'", AgentInvites: true}
	db := openTestStore(t, store)
	mail := &fakeMailer{sent: map[string]string{}}
	hive := NewHive(db, cfg, mail, log.New(io.Discard, "", 0))
	srv := httptest.NewServer(NewServer(cfg, hive).Handler())
	t.Cleanup(srv.Close)
	return &testEnv{t: t, hive: hive, mail: mail, srv: srv}
}

// TestHive runs every behaviour test against both stores, so SQLite and
// Postgres cannot drift apart.
func TestHive(t *testing.T) {
	tests := map[string]func(*testing.T, *testEnv){
		"AgentEnrollsByEmailAndUsesTheAPI":           testAgentEnrollsByEmailAndUsesTheAPI,
		"EnrollmentIsRestrictedAndCodesAreSingleUse": testEnrollmentIsRestrictedAndCodesAreSingleUse,
		"KeysRotateRevokeAndNamesAreOwned":           testKeysRotateRevokeAndNamesAreOwned,
		"APIRejectsBadInput":                         testAPIRejectsBadInput,
		"MCP":                                        testMCP,
		"WebSignInAndAccessControl":                  testWebSignInAndAccessControl,
		"SecurityHeadersAndDiscovery":                testSecurityHeadersAndDiscovery,
		"DocumentedOperationsMatchTheRouter":         testDocumentedOperationsMatchTheRouter,
		"MailFailureIsReported":                      testMailFailureIsReported,
		"AgentManagesItsOwnKeys":                     testAgentManagesItsOwnKeys,
		"InvitesEnrollAndRecover":                    testInvitesEnrollAndRecover,
		"DiscoveryAndPublicRead":                     testDiscoveryAndPublicRead,
		"BlocksAreValidatedOnWrite":                  testBlocksAreValidatedOnWrite,
		"ViewsAreSandboxedAndSelfContained":          testViewsAreSandboxedAndSelfContained,
		"FormsCollectAnswersFromAdmins":              testFormsCollectAnswersFromAdmins,
		"GuideTeachesRichThreads":                    testGuideTeachesRichThreads,
		"ViewOwnsThePageAndGetsNoHouseStyle":         testViewOwnsThePageAndGetsNoHouseStyle,
		"KitsAreSharedAndVersioned":                  testKitsAreSharedAndVersioned,
		"ThreadDataIsSharedAndFollowed":              testThreadDataIsSharedAndFollowed,
		"RepliesAndCardsBringTheirOwnHTML":           testRepliesAndCardsBringTheirOwnHTML,
		"AdminHandsTheFrontPageToAThread":            testAdminHandsTheFrontPageToAThread,
	}
	for name, fn := range tests {
		for _, store := range []string{"sqlite", "postgres"} {
			t.Run(name+"/"+store, func(t *testing.T) { fn(t, newEnv(t, store)) })
		}
	}
}

// call makes a JSON request and decodes the response into out (if not nil).
func (e *testEnv) call(method, path, key string, body, out any) int {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			e.t.Fatalf("%s %s: response is not JSON: %s", method, path, raw)
		}
	}
	return resp.StatusCode
}

// enroll runs the email flow an agent uses to get its own key.
func (e *testEnv) enroll(email, name string) string {
	e.t.Helper()
	if s := e.call("POST", "/api/v1/auth/request", "", map[string]string{"email": email}, nil); s != 202 {
		e.t.Fatalf("auth/request: status %d", s)
	}
	var out struct {
		APIKey string `json:"api_key"`
	}
	in := map[string]string{"email": email, "code": e.mail.code(e.t, email), "agent_name": name}
	if s := e.call("POST", "/api/v1/auth/exchange", "", in, &out); s != 201 {
		e.t.Fatalf("auth/exchange: status %d", s)
	}
	return out.APIKey
}

func testAgentEnrollsByEmailAndUsesTheAPI(t *testing.T, e *testEnv) {
	key := e.enroll("scout@johnnycode.ai", "scout")
	if !strings.HasPrefix(key, "hive_") {
		t.Fatalf("key %q lacks the hive_ prefix", key)
	}
	var me Agent
	if s := e.call("GET", "/api/v1/me", key, nil, &me); s != 200 || me.Name != "scout" || me.Owner != "scout@johnnycode.ai" {
		t.Fatalf("me: status %d, %+v", s, me)
	}

	var thread Thread
	in := ThreadInput{Title: "Migrate billing", Body: "Plan in **three** steps", Tags: []string{"billing", "billing", " "}}
	if s := e.call("POST", "/api/v1/threads", key, in, &thread); s != 201 {
		t.Fatalf("create thread: %d", s)
	}
	if len(thread.Tags) != 1 || thread.AgentName != "scout" {
		t.Fatalf("thread = %+v", thread)
	}

	other := e.enroll("teammate@johnnycode.ai", "teammates-agent")
	var reply Reply
	if s := e.call("POST", "/api/v1/threads/"+thread.ID+"/replies", other, map[string]string{"body": "On it"}, &reply); s != 201 {
		t.Fatalf("reply: %d", s)
	}
	// Only the author may edit or delete.
	if s := e.call("PUT", "/api/v1/threads/"+thread.ID, other, map[string]string{"title": "hijacked"}, nil); s != 403 {
		t.Fatalf("editing another agent's thread: status %d, want 403", s)
	}
	if s := e.call("DELETE", "/api/v1/replies/"+reply.ID, key, nil, nil); s != 403 {
		t.Fatalf("deleting another agent's reply: status %d, want 403", s)
	}

	// Status tags: idempotent, validated, and depends-on needs a target.
	var st StatusTag
	if s := e.call("POST", "/api/v1/threads/"+thread.ID+"/status", other, map[string]string{"tag": "in-progress"}, &st); s != 201 {
		t.Fatalf("set status: %d", s)
	}
	var again StatusTag
	if s := e.call("POST", "/api/v1/threads/"+thread.ID+"/status", other, map[string]string{"tag": "in-progress"}, &again); s != 200 || again.ID != st.ID {
		t.Fatalf("repeating a status: status %d, id %s want %s", s, again.ID, st.ID)
	}
	if s := e.call("POST", "/api/v1/threads/"+thread.ID+"/status", other, map[string]string{"tag": "done"}, nil); s != 400 {
		t.Fatalf("unknown tag: status %d, want 400", s)
	}
	if s := e.call("POST", "/api/v1/threads/"+thread.ID+"/status", other, map[string]string{"tag": "depends-on"}, nil); s != 400 {
		t.Fatalf("depends-on without reference: status %d, want 400", s)
	}
	if s := e.call("POST", "/api/v1/replies/"+reply.ID+"/status", key, map[string]string{"tag": "blocked", "reference_id": thread.ID}, nil); s != 201 {
		t.Fatalf("blocked with reference: status %d", s)
	}

	var full Thread
	e.call("GET", "/api/v1/threads/"+thread.ID, key, nil, &full)
	if len(full.Replies) != 1 || len(full.Statuses) != 1 || len(full.Replies[0].Statuses) != 1 || full.ReplyCount != 1 {
		t.Fatalf("thread detail = %+v", full)
	}
	var list []Thread
	e.call("GET", "/api/v1/threads?status=in-progress&q=billing&tag=billing&agent=scout", key, nil, &list)
	if len(list) != 1 {
		t.Fatalf("filtered list has %d threads, want 1", len(list))
	}
	e.call("GET", "/api/v1/threads?q=100%25", key, nil, &list)
	if len(list) != 0 {
		t.Fatalf("a %% in the search matched %d threads; LIKE wildcards must be escaped", len(list))
	}
	var active ActiveContext
	e.call("GET", "/api/v1/context/active", key, nil, &active)
	if len(active.InProgress) != 1 || len(active.RecentThreads) != 1 {
		t.Fatalf("active context = %+v", active)
	}
	var deps struct{ Dependencies []DependencyEdge }
	e.call("GET", "/api/v1/context/dependencies", key, nil, &deps)
	if len(deps.Dependencies) != 1 || deps.Dependencies[0].DependsOn.Title != "Migrate billing" {
		t.Fatalf("dependencies = %+v", deps)
	}
	var ac AgentContext
	if s := e.call("GET", "/api/v1/context/agent/teammates-agent", key, nil, &ac); s != 200 || len(ac.RecentReplies) != 1 {
		t.Fatalf("agent context by name: status %d, %+v", s, ac)
	}

	if s := e.call("DELETE", "/api/v1/threads/"+thread.ID, key, nil, nil); s != 204 {
		t.Fatalf("delete thread: %d", s)
	}
	var n int
	e.hive.db.QueryRow(`SELECT (SELECT COUNT(*) FROM replies) + (SELECT COUNT(*) FROM status_tags)`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d replies or status tags outlived their thread", n)
	}
}

func testEnrollmentIsRestrictedAndCodesAreSingleUse(t *testing.T, e *testEnv) {

	// An outsider gets the same answer but no mail and no key.
	if s := e.call("POST", "/api/v1/auth/request", "", map[string]string{"email": "eve@example.com"}, nil); s != 202 {
		t.Fatalf("outsider request: status %d, want 202", s)
	}
	if len(e.mail.sent) != 0 {
		t.Fatal("mail was sent to an address outside the allowed domains")
	}
	// A lookalike domain is not the allowed domain.
	e.call("POST", "/api/v1/auth/request", "", map[string]string{"email": "eve@johnnycode.ai.evil.com"}, nil)
	e.call("POST", "/api/v1/auth/request", "", map[string]string{"email": "eve@evil.com, a@johnnycode.ai"}, nil)
	if len(e.mail.sent) != 0 {
		t.Fatalf("mail went to %v", e.mail.sent)
	}

	e.call("POST", "/api/v1/auth/request", "", map[string]string{"email": "Bot@JohnnyCode.ai"}, nil)
	code := e.mail.code(t, "bot@johnnycode.ai")
	exchange := func(code string) int {
		return e.call("POST", "/api/v1/auth/exchange", "", map[string]string{"email": "bot@johnnycode.ai", "code": code, "agent_name": "bot"}, nil)
	}
	if s := exchange("AAAA-AAAA"); s != 401 {
		t.Fatalf("wrong code: status %d, want 401", s)
	}
	if s := exchange(strings.ToLower(code)); s != 201 {
		t.Fatalf("right code: status %d, want 201", s)
	}
	if s := exchange(code); s != 401 {
		t.Fatalf("reused code: status %d, want 401", s)
	}

	// Five wrong guesses burn the code, so the right one no longer works.
	e.call("POST", "/api/v1/auth/request", "", map[string]string{"email": "bot@johnnycode.ai"}, nil)
	code = e.mail.code(t, "bot@johnnycode.ai")
	for i := 0; i < loginCodeTries; i++ {
		exchange("ZZZZ-ZZZZ")
	}
	if s := exchange(code); s != 401 {
		t.Fatalf("code still valid after %d wrong guesses", loginCodeTries)
	}

	// An expired code is refused.
	e.call("POST", "/api/v1/auth/request", "", map[string]string{"email": "bot@johnnycode.ai"}, nil)
	code = e.mail.code(t, "bot@johnnycode.ai")
	e.hive.now = func() time.Time { return time.Now().UTC().Add(loginCodeTTL + time.Second) }
	if s := exchange(code); s != 401 {
		t.Fatalf("expired code: status %d, want 401", s)
	}
}

func testKeysRotateRevokeAndNamesAreOwned(t *testing.T, e *testEnv) {
	first := e.enroll("owner@johnnycode.ai", "builder")
	second := e.enroll("owner@johnnycode.ai", "builder") // same owner, same name: rotation
	if s := e.call("GET", "/api/v1/me", first, nil, nil); s != 401 {
		t.Fatalf("old key after rotation: status %d, want 401", s)
	}
	if s := e.call("GET", "/api/v1/me", second, nil, nil); s != 200 {
		t.Fatalf("new key after rotation: status %d", s)
	}

	// Someone else cannot take the name.
	e.call("POST", "/api/v1/auth/request", "", map[string]string{"email": "teammate@johnnycode.ai"}, nil)
	in := map[string]string{"email": "teammate@johnnycode.ai", "code": e.mail.code(t, "teammate@johnnycode.ai"), "agent_name": "builder"}
	if s := e.call("POST", "/api/v1/auth/exchange", "", in, nil); s != 409 {
		t.Fatalf("taking another owner's agent name: status %d, want 409", s)
	}

	var me Agent
	e.call("GET", "/api/v1/me", second, nil, &me)
	if err := e.hive.RevokeAgent(context.Background(), "teammate@johnnycode.ai", me.ID); asError(err).Status != 403 {
		t.Fatalf("revoking someone else's agent: %v, want forbidden", err)
	}
	if err := e.hive.RevokeAgent(context.Background(), "owner@johnnycode.ai", me.ID); err != nil {
		t.Fatal(err)
	}
	if s := e.call("GET", "/api/v1/me", second, nil, nil); s != 401 {
		t.Fatalf("revoked key: status %d, want 401", s)
	}
	// Taking an owner off the allowlist locks out their agents' keys.
	other := e.enroll("leaver@johnnycode.ai", "leavers-agent")
	e.hive.cfg.AllowedDomains, e.hive.cfg.AllowedEmails = nil, []string{"owner@johnnycode.ai"}
	if s := e.call("GET", "/api/v1/me", other, nil, nil); s != 401 {
		t.Fatalf("key of an owner no longer allowed: status %d, want 401", s)
	}
	if _, _, err := e.hive.EnrollAgent(context.Background(), "owner@johnnycode.ai", "builder", ""); asError(err).Status != 409 {
		t.Fatalf("re-enrolling a revoked name: %v, want conflict", err)
	}
}

func testAPIRejectsBadInput(t *testing.T, e *testEnv) {
	key := e.enroll("scout@johnnycode.ai", "scout")
	for _, tc := range []struct {
		name string
		body any
		want int
	}{
		{"empty title", ThreadInput{Title: " ", Body: "x"}, 400},
		{"long title", ThreadInput{Title: strings.Repeat("a", maxTitleLen+1), Body: "x"}, 400},
		{"too many tags", ThreadInput{Title: "t", Body: "b", Tags: strings.Split("a,b,c,d,e,f,g,h,i,j,k,l,m,n,o,p,q,r,s,t,u", ",")}, 400},
		{"long tag", ThreadInput{Title: "t", Body: "b", Tags: []string{strings.Repeat("a", maxTagLen+1)}}, 400},
		{"huge body", map[string]string{"title": "t", "body": strings.Repeat("a", maxRequestBody)}, 413},
		{"not json", "just a string", 400},
	} {
		if s := e.call("POST", "/api/v1/threads", key, tc.body, nil); s != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, s, tc.want)
		}
	}
	var errBody Error
	if s := e.call("GET", "/api/v1/threads/nope", key, nil, &errBody); s != 404 || errBody.Code != "not_found" {
		t.Errorf("missing thread: status %d, body %+v", s, errBody)
	}
	if s := e.call("GET", "/api/v1/threads", "", nil, &errBody); s != 401 || errBody.Hint == "" {
		t.Errorf("no key: status %d, body %+v", s, errBody)
	}
	if s := e.call("GET", "/api/v1/nonsense", key, nil, &errBody); s != 404 || errBody.Hint == "" {
		t.Errorf("unknown route: status %d, body %+v", s, errBody)
	}
	// Guessing keys gets an address locked out.
	for i := 0; i < 25; i++ {
		e.call("GET", "/api/v1/me", "hive_000000000000_"+strings.Repeat("0", 64), nil, nil)
	}
	if s := e.call("GET", "/api/v1/me", "hive_000000000000_"+strings.Repeat("0", 64), nil, nil); s != 429 {
		t.Errorf("after 25 bad keys: status %d, want 429", s)
	}
}

func testMCP(t *testing.T, e *testEnv) {
	key := e.enroll("scout@johnnycode.ai", "scout")
	rpc := func(key, method string, params any) (int, map[string]any) {
		var out map[string]any
		s := e.call("POST", "/mcp", key, map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params}, &out)
		return s, out
	}
	if s, _ := rpc("", "initialize", nil); s != 401 {
		t.Fatalf("MCP without a key: status %d, want 401", s)
	}
	s, out := rpc(key, "initialize", map[string]any{"protocolVersion": "2025-03-26"})
	result, _ := out["result"].(map[string]any)
	if s != 200 || result["protocolVersion"] != "2025-03-26" {
		t.Fatalf("initialize: status %d, %v", s, out)
	}
	if s := e.call("POST", "/mcp", key, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}, nil); s != 202 {
		t.Fatalf("notification: status %d, want 202", s)
	}
	_, out = rpc(key, "tools/list", nil)
	tools := out["result"].(map[string]any)["tools"].([]any)
	if len(tools) != len(mcpTools) {
		t.Fatalf("tools/list returned %d tools, want %d", len(tools), len(mcpTools))
	}

	// toolCall returns the decoded JSON text of a tool result.
	toolCall := func(name string, args any) (map[string]any, bool) {
		t.Helper()
		_, out := rpc(key, "tools/call", map[string]any{"name": name, "arguments": args})
		result, ok := out["result"].(map[string]any)
		if !ok {
			t.Fatalf("%s: no result: %v", name, out)
		}
		text := result["content"].([]any)[0].(map[string]any)["text"].(string)
		isErr, _ := result["isError"].(bool)
		var decoded map[string]any
		json.Unmarshal([]byte(text), &decoded)
		return decoded, isErr
	}
	thread, isErr := toolCall("create_thread", map[string]any{"title": "From MCP", "body": "hello", "tags": []string{"mcp"}})
	if isErr || thread["title"] != "From MCP" {
		t.Fatalf("create_thread: %v", thread)
	}
	id := thread["id"].(string)
	if _, isErr := toolCall("set_status", map[string]any{"thread_id": id, "tag": "in-progress"}); isErr {
		t.Fatal("set_status failed")
	}
	if _, isErr := toolCall("set_status", map[string]any{"tag": "in-progress"}); !isErr {
		t.Fatal("set_status without a target should be a tool error")
	}
	if _, isErr := toolCall("create_reply", map[string]any{"thread_id": id, "body": "a reply"}); isErr {
		t.Fatal("create_reply failed")
	}
	ctx, _ := toolCall("get_active_context", nil)
	if len(ctx["in_progress"].([]any)) != 1 {
		t.Fatalf("get_active_context: %v", ctx)
	}
	listed, _ := toolCall("list_threads", map[string]any{"q": "MCP", "per_page": 5})
	if listed["total"].(float64) != 1 {
		t.Fatalf("list_threads: %v", listed)
	}
	if _, isErr := toolCall("get_thread", map[string]any{"thread_id": "missing"}); !isErr {
		t.Fatal("get_thread on a missing id should be a tool error")
	}
	// Every tool must at least run without a protocol error.
	for _, tool := range mcpTools {
		_, out := rpc(key, "tools/call", map[string]any{"name": tool.Name, "arguments": map[string]any{}})
		if out["error"] != nil {
			t.Errorf("tool %s: protocol error %v", tool.Name, out["error"])
		}
	}
}

func testWebSignInAndAccessControl(t *testing.T, e *testEnv) {
	// The test server is plain HTTP, so use the non-Secure cookie.
	cfg := e.hive.cfg
	cfg.BaseURL = e.srv.URL
	e.hive.cfg = cfg
	e.srv.Config.Handler = NewServer(cfg, e.hive).Handler()

	newBrowser := func() *http.Client {
		jar, _ := cookiejar.New(nil)
		return &http.Client{Jar: jar}
	}
	get := func(c *http.Client, path string) (int, string) {
		resp, err := c.Get(e.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	post := func(c *http.Client, path string, form url.Values, headers ...string) (int, string) {
		req, _ := http.NewRequest("POST", e.srv.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		for i := 0; i+1 < len(headers); i += 2 {
			req.Header.Set(headers[i], headers[i+1])
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	signIn := func(email string) *http.Client {
		c := newBrowser()
		post(c, "/login", url.Values{"email": {email}})
		_, body := post(c, "/login/verify", url.Values{"email": {email}, "code": {e.mail.code(t, email)}})
		if !strings.Contains(body, "Activity") {
			t.Fatalf("signing in as %s did not reach the dashboard", email)
		}
		return c
	}

	anon := newBrowser()
	if _, body := get(anon, "/dashboard"); !strings.Contains(body, "Sign in") {
		t.Fatal("the dashboard is visible without a session")
	}
	// Opening the emailed link must not sign anyone in or spend the code.
	post(anon, "/login", url.Values{"email": {"teammate@johnnycode.ai"}})
	code := e.mail.code(t, "teammate@johnnycode.ai")
	get(anon, "/login/verify?email=teammate@johnnycode.ai&code="+code)
	if _, body := get(anon, "/dashboard"); !strings.Contains(body, "Sign in") {
		t.Fatal("a GET of the sign-in link created a session")
	}
	if _, body := post(anon, "/login/verify", url.Values{"email": {"teammate@johnnycode.ai"}, "code": {code}}); !strings.Contains(body, "Activity") {
		t.Fatal("the code was spent by the GET")
	}
	member := anon

	// A member creates a key in the UI and it works.
	_, body := post(member, "/agents", url.Values{"name": {"teammates-claude"}})
	key := regexp.MustCompile(`hive_[0-9a-f]{12}_[0-9a-f]{64}`).FindString(body)
	if key == "" {
		t.Fatal("creating an agent did not show a key")
	}
	if s := e.call("GET", "/api/v1/me", key, nil, nil); s != 200 {
		t.Fatalf("key from the UI: status %d", s)
	}

	// Agent-written Markdown cannot inject script into the dashboard.
	var thread Thread
	e.call("POST", "/api/v1/threads", key, ThreadInput{Title: "<script>alert(1)</script>",
		Body: "<script>alert(2)</script> [x](javascript:alert(3)) <img src=x onerror=alert(4)>"}, &thread)
	_, body = get(member, "/dashboard/threads/"+thread.ID)
	for _, bad := range []string{"<script>alert", "javascript:alert", "onerror=alert"} {
		if strings.Contains(body, bad) {
			t.Errorf("thread page contains unescaped %q", bad)
		}
	}

	// Admin pages are for ADMIN_EMAILS only.
	if s, _ := get(member, "/admin"); s != 404 {
		t.Fatalf("non-admin on /admin: status %d, want 404", s)
	}
	if s, _ := post(member, "/admin/threads/"+thread.ID+"/delete", nil); s != 404 {
		t.Fatalf("non-admin deleting a thread: status %d, want 404", s)
	}
	admin := signIn("owner@johnnycode.ai")
	if s, _ := get(admin, "/admin"); s != 200 {
		t.Fatalf("admin on /admin: status %d", s)
	}
	// A forged cross-site form post is refused even with the admin's cookie.
	if s, _ := post(admin, "/admin/threads/"+thread.ID+"/delete", nil, "Sec-Fetch-Site", "cross-site"); s != 403 {
		t.Fatalf("cross-site POST: status %d, want 403", s)
	}
	if s, _ := post(admin, "/admin/threads/"+thread.ID+"/archive", nil, "Origin", e.srv.URL); s != 200 {
		t.Fatalf("admin archiving a thread: status %d", s)
	}
	if s := e.call("POST", "/api/v1/threads/"+thread.ID+"/replies", key, map[string]string{"body": "late"}, nil); s != 409 {
		t.Fatalf("replying to an archived thread: status %d, want 409", s)
	}
	post(admin, "/admin/threads/"+thread.ID+"/delete", nil)
	if s := e.call("GET", "/api/v1/threads/"+thread.ID, key, nil, nil); s != 404 {
		t.Fatalf("thread still exists after admin delete: status %d", s)
	}

	// Signing out ends the session server-side.
	post(member, "/logout", nil)
	if _, body := get(member, "/agents"); !strings.Contains(body, "Sign in") {
		t.Fatal("session survived sign-out")
	}
}

func testSecurityHeadersAndDiscovery(t *testing.T, e *testEnv) {
	resp, err := http.Get(e.srv.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for header, want := range map[string]string{
		"Content-Security-Policy":   "default-src 'none'",
		"X-Content-Type-Options":    "nosniff",
		"X-Frame-Options":           "DENY",
		"Referrer-Policy":           "no-referrer",
		"Strict-Transport-Security": "max-age=",
	} {
		if got := resp.Header.Get(header); !strings.Contains(got, want) {
			t.Errorf("%s = %q, want it to contain %q", header, got, want)
		}
	}
	for _, path := range []string{"/healthz", "/llms.txt", "/openapi.json", "/api/v1", "/static/style.css", "/robots.txt"} {
		resp, err := http.Get(e.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("GET %s: status %d", path, resp.StatusCode)
		}
	}
}

// Every documented operation must be a real route, and every /api/v1 route
// must be documented.
func testDocumentedOperationsMatchTheRouter(t *testing.T, e *testEnv) {
	srv := NewServer(e.hive.cfg, e.hive)
	mux := http.NewServeMux()
	srv.apiRoutes(mux)
	for _, op := range operations {
		req := httptest.NewRequest(op.Method, strings.ReplaceAll(op.Path, "{id}", "x"), nil)
		if _, pattern := mux.Handler(req); pattern != op.Method+" "+op.Path {
			t.Errorf("%s %s is documented but routes to %q", op.Method, op.Path, pattern)
		}
	}
	spec, _ := json.Marshal(srv.openAPI())
	if !json.Valid(spec) || !strings.Contains(srv.llmsTxt(), "get_active_context") {
		t.Error("discovery documents are malformed")
	}
}

func TestAdoptsAnUnversionedDatabaseAndItsKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte("af_legacy-key-from-the-old-release"), bcrypt.MinCost)
	if _, err := old.Exec(sqliteMigrations[0] + `
		CREATE TABLE users (id TEXT PRIMARY KEY, username TEXT NOT NULL UNIQUE, password_hash TEXT NOT NULL, created_at DATETIME);
		INSERT INTO agents (id, name, owner, api_key_hash) VALUES ('a1', 'veteran', 'someone', '` + string(hash) + `');
		INSERT INTO threads (id, agent_id, title, body) VALUES ('t1', 'a1', 'Old thread', 'still here');`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	db, err := OpenDB(Config{DBPath: path})
	if err != nil {
		t.Fatalf("opening a pre-versioning database: %v", err)
	}
	defer db.Close()
	hive := NewHive(db, Config{}, nil, log.New(io.Discard, "", 0))
	ctx := context.Background()
	for i := 0; i < 2; i++ { // second pass exercises the cache
		a, err := hive.Authenticate(ctx, "af_legacy-key-from-the-old-release", "1.2.3.4")
		if err != nil || a.Name != "veteran" {
			t.Fatalf("legacy key: %v %+v", err, a)
		}
	}
	if _, err := hive.Authenticate(ctx, "af_some-other-legacy-looking-key", "1.2.3.4"); err == nil {
		t.Fatal("a wrong legacy key was accepted")
	}
	if thread, err := hive.GetThread(ctx, "t1", false); err != nil || thread.Title != "Old thread" {
		t.Fatalf("old thread: %v", err)
	}
	if err := hive.RevokeAgent(ctx, "someone", "a1"); err != nil {
		t.Fatal(err)
	}
	if _, err := hive.Authenticate(ctx, "af_legacy-key-from-the-old-release", "1.2.3.4"); err == nil {
		t.Fatal("a revoked legacy key was accepted")
	}
}

func TestConfigRefusesUnsafeSettings(t *testing.T) {
	good := Config{BaseURL: "https://hive.johnnycode.ai", AllowedDomains: []string{"johnnycode.ai"},
		RemailAPIKey: "rk_x", RemailFrom: "hive@johnnycode.ai", AdminEmails: []string{"owner@johnnycode.ai"}}
	if err := good.validate(); err != nil {
		t.Fatalf("a complete config was refused: %v", err)
	}
	for name, change := range map[string]func(*Config){
		"plain http":        func(c *Config) { c.BaseURL = "http://hive.johnnycode.ai" },
		"no base url":       func(c *Config) { c.BaseURL = "" },
		"base url path":     func(c *Config) { c.BaseURL = "https://johnnycode.ai/hive" },
		"no remail key":     func(c *Config) { c.RemailAPIKey = "" },
		"no from address":   func(c *Config) { c.RemailFrom = "" },
		"nobody allowed":    func(c *Config) { c.AllowedDomains = nil },
		"admin not allowed": func(c *Config) { c.AdminEmails = []string{"root@example.com"} },
	} {
		c := good
		change(&c)
		if c.validate() == nil {
			t.Errorf("%s: config was accepted", name)
		}
	}
	dev := Config{BaseURL: "http://localhost:8080", AllowedDomains: []string{"johnnycode.ai"}, DevMode: true}
	if err := dev.validate(); err != nil {
		t.Errorf("DEV_MODE config refused: %v", err)
	}
}

func TestRemailMailerSpeaksTheRemailAPI(t *testing.T) {
	var got struct {
		auth, idem string
		body       map[string]string
	}
	status := 200
	remail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.auth, got.idem = r.Header.Get("Authorization"), r.Header.Get("Idempotency-Key")
		json.NewDecoder(r.Body).Decode(&got.body)
		if r.URL.Path != "/v1/emails" || r.Method != "POST" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(status)
		if status != 200 {
			io.WriteString(w, `{"error":{"code":"domain_not_verified","message":"verify the domain first"}}`)
		}
	}))
	defer remail.Close()
	m := NewRemailMailer(Config{RemailBaseURL: remail.URL, RemailAPIKey: "rk_test", RemailFrom: "hive@johnnycode.ai"})
	if err := m.Send(context.Background(), "a@johnnycode.ai", "subject", "text"); err != nil {
		t.Fatal(err)
	}
	if got.auth != "Bearer rk_test" || got.idem == "" || got.body["from"] != "hive@johnnycode.ai" ||
		got.body["to"] != "a@johnnycode.ai" || got.body["text"] != "text" {
		t.Fatalf("request = %+v", got)
	}
	status = 422
	if err := m.Send(context.Background(), "a@johnnycode.ai", "s", "t"); err == nil || !strings.Contains(err.Error(), "domain_not_verified") {
		t.Fatalf("a Remail refusal should surface its code, got %v", err)
	}
}

func testMailFailureIsReported(t *testing.T, e *testEnv) {
	e.mail.fail = true
	var body Error
	if s := e.call("POST", "/api/v1/auth/request", "", map[string]string{"email": "a@johnnycode.ai"}, &body); s != 502 || body.Code != "mail_failed" {
		t.Fatalf("status %d, body %+v", s, body)
	}
}

// legacySQLite writes a database as the pre-versioning release left it: one
// agent with a bcrypt key, a thread, a reply, a status tag, and one reply
// whose thread is gone (that release did not enforce foreign keys).
func legacySQLite(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "forum.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	hash, _ := bcrypt.GenerateFromPassword([]byte("af_legacy-key-from-the-old-release"), bcrypt.MinCost)
	if _, err := old.Exec(sqliteMigrations[0] + `
		INSERT INTO agents (id, name, owner, api_key_hash) VALUES ('a1', 'veteran', 'someone', '` + string(hash) + `');
		INSERT INTO threads (id, agent_id, title, body, tags, pinned) VALUES ('t1', 'a1', 'Old thread', 'still here', '["legacy"]', 1);
		INSERT INTO replies (id, thread_id, agent_id, body) VALUES ('r1', 't1', 'a1', 'old reply');
		INSERT INTO replies (id, thread_id, agent_id, body) VALUES ('r-orphan', 'gone', 'a1', 'thread was deleted');
		INSERT INTO status_tags (id, thread_id, agent_id, tag) VALUES ('s1', 't1', 'a1', 'in-progress');
		INSERT INTO announcements (id, title, body) VALUES ('n1', 'Hello', 'world');`); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestImportSQLiteIntoPostgres(t *testing.T) {
	target := openTestStore(t, "postgres")
	source := legacySQLite(t)
	before, _ := os.ReadFile(source)
	ctx := context.Background()

	counts, err := copySQLiteInto(ctx, source, target)
	if err != nil {
		t.Fatal(err)
	}
	if counts.agents != 1 || counts.threads != 1 || counts.replies != 1 || counts.statuses != 1 || counts.announcements != 1 || counts.skipped != 1 {
		t.Fatalf("counts = %+v", counts)
	}
	if after, _ := os.ReadFile(source); !bytes.Equal(before, after) {
		t.Fatal("the import changed the source file")
	}

	hive := NewHive(target, Config{}, nil, log.New(io.Discard, "", 0))
	agent, err := hive.Authenticate(ctx, "af_legacy-key-from-the-old-release", "1.2.3.4")
	if err != nil || agent.Name != "veteran" {
		t.Fatalf("legacy key after import: %v", err)
	}
	thread, err := hive.GetThread(ctx, "t1", false)
	if err != nil || !thread.Pinned || len(thread.Replies) != 1 || len(thread.Statuses) != 1 || thread.Tags[0] != "legacy" {
		t.Fatalf("imported thread = %+v, %v", thread, err)
	}
	// The imported agent can keep working in the new store.
	if _, err := hive.CreateReply(ctx, agent, "t1", ReplyInput{Body: "new reply"}); err != nil {
		t.Fatal(err)
	}
	// A second import must not pile onto existing data.
	if _, err := copySQLiteInto(ctx, source, target); err == nil {
		t.Fatal("importing into a non-empty database was allowed")
	}
}

func TestMintKeyUsesTheConfiguredStore(t *testing.T) {
	for _, store := range []string{"sqlite", "postgres"} {
		t.Run(store, func(t *testing.T) {
			db := openTestStore(t, store)
			if store == "postgres" {
				t.Setenv("DATABASE_URL", os.Getenv("TEST_STORE_URL"))
			} else {
				db.Close()
				t.Setenv("DATABASE_URL", "")
				t.Setenv("DB_PATH", filepath.Join(t.TempDir(), "mint.db"))
			}
			t.Setenv("ALLOWED_EMAIL_DOMAINS", "johnnycode.ai")
			var out bytes.Buffer
			if err := mintKey(log.New(io.Discard, "", 0), &out, []string{"-owner", "owner@johnnycode.ai", "-name", "minted"}); err != nil {
				t.Fatal(err)
			}
			cfg, _ := LoadConfig()
			check, err := OpenDB(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer check.Close()
			hive := NewHive(check, cfg, nil, log.New(io.Discard, "", 0))
			agent, err := hive.Authenticate(context.Background(), strings.TrimSpace(out.String()), "1.2.3.4")
			if err != nil || agent.Name != "minted" || agent.Owner != "owner@johnnycode.ai" {
				t.Fatalf("minted key: %v %+v", err, agent)
			}
		})
	}
}

func TestBadDatabaseURLIsRefusedWithoutLeakingIt(t *testing.T) {
	// Nothing listens on port 1, and the password must not reach the log.
	sqlitePath := filepath.Join(t.TempDir(), "x.db")
	_, err := OpenDB(Config{DatabaseURL: "postgres://hive:s3cret-pw@127.0.0.1:1/hive?connect_timeout=2", DBPath: sqlitePath})
	if err == nil {
		t.Fatal("an unreachable DATABASE_URL was accepted")
	}
	if msg := err.Error(); !strings.Contains(msg, "cannot use DATABASE_URL") || strings.Contains(msg, "s3cret-pw") {
		t.Fatalf("error = %q", msg)
	}
	if _, statErr := os.Stat(sqlitePath); statErr == nil {
		t.Fatal("fell back to SQLite")
	}
	bad := Config{BaseURL: "https://h.example", AllowedDomains: []string{"johnnycode.ai"}, RemailAPIKey: "k", RemailFrom: "f@johnnycode.ai",
		DatabaseURL: "mysql://nope"}
	if err := bad.validate(); err == nil || !strings.HasPrefix(err.Error(), "refusing to start:") || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("a non-Postgres DATABASE_URL: %v", err)
	}
}

func testAgentManagesItsOwnKeys(t *testing.T, e *testEnv) {
	first := e.enroll("owner@johnnycode.ai", "worker")

	// A second key for the same agent leaves the first valid.
	var minted struct {
		APIKey string `json:"api_key"`
		Key    APIKey `json:"key"`
	}
	if s := e.call("POST", "/api/v1/keys", first, map[string]string{"label": "ci"}, &minted); s != 201 || minted.APIKey == "" {
		t.Fatalf("mint a second key: status %d", s)
	}
	second := minted.APIKey
	for _, key := range []string{first, second} {
		var me Agent
		if s := e.call("GET", "/api/v1/me", key, nil, &me); s != 200 || me.Name != "worker" {
			t.Fatalf("after minting, a key stopped working: status %d", s)
		}
	}
	var keys []APIKey
	e.call("GET", "/api/v1/keys", second, nil, &keys)
	if len(keys) != 2 || keys[0].Label != "default" || keys[0].Current || !keys[1].Current {
		t.Fatalf("keys as seen by the second key = %+v", keys)
	}
	body, _ := json.Marshal(keys)
	if strings.Contains(string(body), "hive_") {
		t.Fatal("the key list contains a secret")
	}
	if s := e.call("POST", "/api/v1/keys", first, map[string]string{"label": " "}, nil); s != 400 {
		t.Fatalf("a key without a label: status %d, want 400", s)
	}

	// Rotating replaces exactly that key.
	if s := e.call("POST", "/api/v1/keys/"+minted.Key.ID+"/rotate", first, nil, &minted); s != 201 {
		t.Fatalf("rotate: status %d", s)
	}
	if s := e.call("GET", "/api/v1/me", second, nil, nil); s != 401 {
		t.Fatalf("the rotated-out key: status %d, want 401", s)
	}
	third := minted.APIKey
	if s := e.call("GET", "/api/v1/me", third, nil, nil); s != 200 {
		t.Fatalf("the key from a rotation: status %d", s)
	}

	// Revoking one key ends that key only.
	if s := e.call("DELETE", "/api/v1/keys/"+minted.Key.ID, first, nil, nil); s != 204 {
		t.Fatalf("revoke: status %d", s)
	}
	if s := e.call("GET", "/api/v1/me", third, nil, nil); s != 401 {
		t.Fatalf("a revoked key: status %d, want 401", s)
	}
	if s := e.call("GET", "/api/v1/me", first, nil, nil); s != 200 {
		t.Fatalf("revoking one key broke another: status %d", s)
	}
	// The last key cannot be revoked, and one agent cannot touch another's keys.
	e.call("GET", "/api/v1/keys", first, nil, &keys)
	if len(keys) != 1 {
		t.Fatalf("%d keys left, want 1", len(keys))
	}
	if s := e.call("DELETE", "/api/v1/keys/"+keys[0].ID, first, nil, nil); s != 409 {
		t.Fatalf("revoking the last key: status %d, want 409", s)
	}
	other := e.enroll("teammate@johnnycode.ai", "stranger")
	if s := e.call("DELETE", "/api/v1/keys/"+keys[0].ID, other, nil, nil); s != 404 {
		t.Fatalf("revoking another agent's key: status %d, want 404", s)
	}
	if s := e.call("POST", "/api/v1/keys/"+keys[0].ID+"/rotate", other, nil, nil); s != 404 {
		t.Fatalf("rotating another agent's key: status %d, want 404", s)
	}

	// Enrolling again is recovery: every earlier key dies.
	e.call("POST", "/api/v1/keys", first, map[string]string{"label": "spare"}, &minted)
	recovered := e.enroll("owner@johnnycode.ai", "worker")
	for _, key := range []string{first, minted.APIKey} {
		if s := e.call("GET", "/api/v1/me", key, nil, nil); s != 401 {
			t.Fatalf("a key from before re-enrollment: status %d, want 401", s)
		}
	}
	if s := e.call("GET", "/api/v1/me", recovered, nil, nil); s != 200 {
		t.Fatalf("the key from re-enrollment: status %d", s)
	}
}

func testInvitesEnrollAndRecover(t *testing.T, e *testEnv) {
	inviter := e.enroll("owner@johnnycode.ai", "second")
	type invited struct {
		Invite  string `json:"invite"`
		Details Invite `json:"details"`
	}
	type enrolled struct {
		APIKey string `json:"api_key"`
		Agent  Agent  `json:"agent"`
	}
	var inv invited
	if s := e.call("POST", "/api/v1/invites", inviter, map[string]string{"agent_name": "Third"}, &inv); s != 201 || !strings.HasPrefix(inv.Invite, "hiveinv_") {
		t.Fatalf("create invite: status %d, %+v", s, inv)
	}

	// The third agent joins with the invite alone: no mailbox, no key.
	mailsBefore := len(e.mail.sent)
	var third enrolled
	if s := e.call("POST", "/api/v1/auth/redeem", "", map[string]string{"invite": inv.Invite}, &third); s != 201 {
		t.Fatalf("redeem: status %d", s)
	}
	if len(e.mail.sent) != mailsBefore {
		t.Fatal("redeeming an invite sent mail")
	}
	if third.Agent.Name != "third" || third.Agent.Owner != "owner@johnnycode.ai" || third.Agent.InvitedBy != "second" {
		t.Fatalf("invited agent = %+v", third.Agent)
	}
	if s := e.call("POST", "/api/v1/threads", third.APIKey, ThreadInput{Title: "I was invited", Body: "hello"}, nil); s != 201 {
		t.Fatalf("the invited agent posting: status %d", s)
	}
	// An invite works once.
	if s := e.call("POST", "/api/v1/auth/redeem", "", map[string]string{"invite": inv.Invite}, nil); s != 401 {
		t.Fatalf("reusing an invite: status %d, want 401", s)
	}
	if s := e.call("POST", "/api/v1/auth/redeem", "", map[string]string{"invite": "hiveinv_" + strings.Repeat("0", 64)}, nil); s != 401 {
		t.Fatalf("a made-up invite: status %d, want 401", s)
	}
	var issued []Invite
	e.call("GET", "/api/v1/invites", inviter, nil, &issued)
	if len(issued) != 1 || issued[0].UsedAt == nil {
		t.Fatalf("issued invites = %+v", issued)
	}

	// Recovery without a mailbox: a new invite for the same name resets its keys.
	e.call("POST", "/api/v1/invites", inviter, map[string]string{"agent_name": "third"}, &inv)
	var again enrolled
	if s := e.call("POST", "/api/v1/auth/redeem", "", map[string]string{"invite": inv.Invite}, &again); s != 201 || again.Agent.ID != third.Agent.ID {
		t.Fatalf("recovery by invite: status %d, agent %+v", s, again.Agent)
	}
	if s := e.call("GET", "/api/v1/me", third.APIKey, nil, nil); s != 401 {
		t.Fatalf("the lost key after recovery: status %d, want 401", s)
	}
	if s := e.call("GET", "/api/v1/me", again.APIKey, nil, nil); s != 200 {
		t.Fatalf("the recovered key: status %d", s)
	}

	// An invite cannot take a name that belongs to another owner.
	e.enroll("teammate@johnnycode.ai", "theirs")
	if s := e.call("POST", "/api/v1/invites", inviter, map[string]string{"agent_name": "theirs"}, nil); s != 409 {
		t.Fatalf("inviting another owner's agent name: status %d, want 409", s)
	}
	if s := e.call("POST", "/api/v1/invites", inviter, map[string]string{"agent_name": "x"}, nil); s != 400 {
		t.Fatalf("an invalid name: status %d, want 400", s)
	}

	// Unused invites can be cancelled, expire, and die with their inviter.
	e.call("POST", "/api/v1/invites", inviter, map[string]string{"agent_name": "cancelled"}, &inv)
	if s := e.call("DELETE", "/api/v1/invites/"+inv.Details.ID, again.APIKey, nil, nil); s != 404 {
		t.Fatalf("cancelling someone else's invite: status %d, want 404", s)
	}
	if s := e.call("DELETE", "/api/v1/invites/"+inv.Details.ID, inviter, nil, nil); s != 204 {
		t.Fatalf("cancelling an invite: status %d", s)
	}
	if s := e.call("POST", "/api/v1/auth/redeem", "", map[string]string{"invite": inv.Invite}, nil); s != 401 {
		t.Fatalf("a cancelled invite: status %d, want 401", s)
	}
	e.call("POST", "/api/v1/invites", inviter, map[string]string{"agent_name": "orphan"}, &inv)
	var me Agent
	e.call("GET", "/api/v1/me", inviter, nil, &me)
	if err := e.hive.RevokeAgent(context.Background(), "owner@johnnycode.ai", me.ID); err != nil {
		t.Fatal(err)
	}
	if s := e.call("POST", "/api/v1/auth/redeem", "", map[string]string{"invite": inv.Invite}, nil); s != 401 {
		t.Fatalf("an invite from a revoked agent: status %d, want 401", s)
	}
	e.call("POST", "/api/v1/invites", again.APIKey, map[string]string{"agent_name": "late"}, &inv)
	e.hive.now = func() time.Time { return time.Now().UTC().Add(inviteTTL + time.Minute) }
	if s := e.call("POST", "/api/v1/auth/redeem", "", map[string]string{"invite": inv.Invite}, nil); s != 401 {
		t.Fatalf("an expired invite: status %d, want 401", s)
	}

	// The operator can turn invites off.
	e.hive.now = func() time.Time { return time.Now().UTC() }
	e.hive.cfg.AgentInvites = false
	if s := e.call("POST", "/api/v1/invites", again.APIKey, map[string]string{"agent_name": "nobody"}, nil); s != 403 {
		t.Fatalf("inviting with invites off: status %d, want 403", s)
	}
}

func testDiscoveryAndPublicRead(t *testing.T, e *testEnv) {
	fetch := func(path, accept string) (int, string) {
		req, _ := http.NewRequest("GET", e.srv.URL+path, nil)
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		resp, err := http.DefaultTransport.RoundTrip(req) // no redirect following
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	// An agent that knows only the URL gets the guide; a browser gets the sign-in page.
	if s, body := fetch("/", "*/*"); s != 200 || !strings.Contains(body, "/api/v1/auth/redeem") || !strings.Contains(body, "If you lose your key") {
		t.Fatalf("GET / as an agent: status %d", s)
	}
	if s, _ := fetch("/", ""); s != 200 {
		t.Fatalf("GET / with no Accept header: status %d, want the guide", s)
	}
	if s, _ := fetch("/", "text/html,application/xhtml+xml"); s != 303 {
		t.Fatalf("GET / as a browser: status %d, want a redirect to sign-in", s)
	}

	key := e.enroll("owner@johnnycode.ai", "reader")
	e.call("POST", "/api/v1/threads", key, ThreadInput{Title: "Readable", Body: "body"}, nil)

	// Private by default: reads need a key, and the guide says so.
	var discovery struct {
		PublicRead bool `json:"public_read"`
	}
	if s := e.call("GET", "/api/v1", "", nil, &discovery); s != 200 || discovery.PublicRead {
		t.Fatalf("discovery: status %d, %+v", s, discovery)
	}
	var errBody Error
	if s := e.call("GET", "/api/v1/threads", "", nil, &errBody); s != 401 || !strings.Contains(errBody.Hint, "/llms.txt") {
		t.Fatalf("private read without a key: status %d, hint %q", s, errBody.Hint)
	}
	if _, guide := fetch("/llms.txt", ""); !strings.Contains(guide, "Every API call needs a key, reads included") || strings.Contains(guide, "[no key]: List threads") {
		t.Fatal("the guide does not describe private reads")
	}

	// With PUBLIC_READ=1 reads work without a key, writes do not, and the guide changes.
	cfg := e.hive.cfg
	cfg.PublicRead = true
	e.hive.cfg = cfg
	e.srv.Config.Handler = NewServer(cfg, e.hive).Handler()
	var threads []Thread
	if s := e.call("GET", "/api/v1/threads", "", nil, &threads); s != 200 || len(threads) != 1 {
		t.Fatalf("public read: status %d, %d threads", s, len(threads))
	}
	var active ActiveContext
	if s := e.call("GET", "/api/v1/context/active", "", nil, &active); s != 200 || len(active.RecentThreads) != 1 {
		t.Fatalf("public context: status %d", s)
	}
	var agents []Agent
	e.call("GET", "/api/v1/agents", "", nil, &agents)
	if len(agents) != 1 || agents[0].Owner != "" {
		t.Fatalf("public agent list leaks owners: %+v", agents)
	}
	e.call("GET", "/api/v1/agents", key, nil, &agents)
	if agents[0].Owner != "owner@johnnycode.ai" {
		t.Fatalf("owners hidden from an authenticated agent: %+v", agents)
	}
	for _, write := range []string{"/api/v1/threads", "/api/v1/keys", "/api/v1/invites"} {
		if s := e.call("POST", write, "", map[string]string{"title": "x", "body": "y"}, nil); s != 401 {
			t.Fatalf("public POST %s: status %d, want 401", write, s)
		}
	}
	if s := e.call("GET", "/api/v1/me", "", nil, nil); s != 401 {
		t.Fatalf("GET /me without a key: status %d, want 401", s)
	}
	if s := e.call("GET", "/api/v1/threads", "hive_000000000000_"+strings.Repeat("0", 64), nil, nil); s != 401 {
		t.Fatalf("public read with a wrong key: status %d, want 401", s)
	}
	if _, guide := fetch("/llms.txt", ""); !strings.Contains(guide, "Reading is public") || !strings.Contains(guide, "GET /api/v1/threads [no key]") {
		t.Fatal("the guide does not describe public reads")
	}
}

// A store written by the previous release, with one key per agent kept on
// the agent row, must come through the move to agent_keys with that key
// still valid. This is what happens to the live database on deploy.
func TestUpgradeKeepsExistingKeysWorking(t *testing.T) {
	key, keyID, hash := newAPIKey()
	seed := `INSERT INTO agents (id, name, owner, api_key_hash, key_id, created_at, last_seen_at)
		VALUES ('a1', 'incumbent', 'owner@johnnycode.ai', '` + hash + `', '` + keyID + `', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP);
		INSERT INTO agents (id, name, owner, api_key_hash, key_id, created_at, last_seen_at, revoked_at)
		VALUES ('a2', 'gone', 'owner@johnnycode.ai', '', NULL, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP);`
	stores := map[string]func(t *testing.T) Config{
		"sqlite": func(t *testing.T) Config {
			path := filepath.Join(t.TempDir(), "previous.db")
			old, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer old.Close()
			if _, err := old.Exec(sqliteMigrations[0] + sqliteMigrations[1] + seed + "PRAGMA user_version = 2;"); err != nil {
				t.Fatal(err)
			}
			return Config{DBPath: path}
		},
		"postgres": func(t *testing.T) Config {
			dsn := testPostgresURL(t)
			old, err := sql.Open("pgx", dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer old.Close()
			if _, err := old.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now());
				INSERT INTO schema_migrations (version) VALUES (1);` + postgresMigrations[0] + seed); err != nil {
				t.Fatal(err)
			}
			return Config{DatabaseURL: dsn}
		},
	}
	for store, prepare := range stores {
		t.Run(store, func(t *testing.T) {
			cfg := prepare(t)
			cfg.AllowedDomains = []string{"johnnycode.ai"}
			db, err := OpenDB(cfg)
			if err != nil {
				t.Fatalf("upgrading: %v", err)
			}
			defer db.Close()
			hive := NewHive(db, cfg, nil, log.New(io.Discard, "", 0))
			ctx := context.Background()
			agent, err := hive.Authenticate(ctx, key, "1.2.3.4")
			if err != nil || agent.Name != "incumbent" {
				t.Fatalf("the existing key after the upgrade: %v", err)
			}
			keys, err := hive.ListKeys(ctx, agent)
			if err != nil || len(keys) != 1 || keys[0].ID != keyID || !keys[0].Current {
				t.Fatalf("keys after the upgrade = %+v, %v", keys, err)
			}
			// Opening it again must be a no-op.
			again, err := OpenDB(cfg)
			if err != nil {
				t.Fatalf("second start: %v", err)
			}
			again.Close()
		})
	}
}
