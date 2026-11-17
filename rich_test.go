package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

// These tests cover rich threads: blocks the hive draws, sandboxed HTML
// views with what they are allowed to reach, and forms people answer.

const fence = "```"

func hiveBlock(kind, jsonBody string) string {
	return fence + "hive:" + kind + "\n" + jsonBody + "\n" + fence
}

// useHTTP rebuilds the server for the plain-HTTP test address, so that
// browser sessions (non-Secure cookies) work.
func (e *testEnv) useHTTP() {
	cfg := e.hive.cfg
	cfg.BaseURL = e.srv.URL
	e.hive.cfg = cfg
	e.srv.Config.Handler = NewServer(cfg, e.hive).Handler()
}

// browser signs a person in and returns their client. It does not follow
// redirects, so a test can see them.
func (e *testEnv) browser(email string) *http.Client {
	e.t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	post := func(path string, form url.Values) int {
		resp, err := c.PostForm(e.srv.URL+path, form)
		if err != nil {
			e.t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	post("/login", url.Values{"email": {email}})
	if s := post("/login/verify", url.Values{"email": {email}, "code": {e.mail.code(e.t, email)}}); s != http.StatusSeeOther {
		e.t.Fatalf("signing in as %s: status %d", email, s)
	}
	return c
}

// do makes a browser request; body is a form (url.Values), JSON (string) or nil.
func (e *testEnv) do(c *http.Client, method, path string, body any, headers ...string) (int, http.Header, string) {
	e.t.Helper()
	var rd io.Reader
	ctype := ""
	switch b := body.(type) {
	case url.Values:
		rd, ctype = strings.NewReader(b.Encode()), "application/x-www-form-urlencoded"
	case string:
		rd, ctype = strings.NewReader(b), "application/json"
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(raw)
}

func TestBlocksRender(t *testing.T) {
	for _, b := range blockTypes {
		out, err := renderBlock(b.Name, []byte(b.Example))
		if err != nil || !strings.Contains(out, `class="hb `) {
			t.Errorf("the documented example of hive:%s does not render: %v", b.Name, err)
		}
	}
	page := string(renderMarkdown("**Headline**\nits paragraph\n\n| a | b |\n|---|--:|\n| 1 | 2 |\n\n<script>alert(1)</script>\n\n" +
		hiveBlock("chart", `{"x":["a","b"],"series":[{"name":"<img src=x onerror=alert(1)>","data":[1,null]}]}`)))
	for _, want := range []string{"<strong>Headline</strong><br>", "<table>", `align="right"`, "<svg", "&lt;img src=x"} {
		if !strings.Contains(page, want) {
			t.Errorf("rendered body lacks %q:\n%s", want, page)
		}
	}
	for _, bad := range []string{"<script>", "<img src=x", "style="} {
		if strings.Contains(page, bad) {
			t.Errorf("rendered body contains %q: agent text reached the page as markup, or needs an inline style", bad)
		}
	}
	// A javascript: link in a block is refused, not shown.
	if _, err := renderBlock("cards", []byte(`{"items":[{"title":"x","url":"javascript:alert(1)"}]}`)); err == nil {
		t.Error("a javascript: url was accepted in a cards block")
	}
	for kind, body := range map[string]string{
		"chart":    `{"x":["a"],"series":[{"data":[1,2]}]}`,  // lengths differ
		"metrics":  `{"items":[{"label":"x","valeu":1}]}`,    // misspelt field
		"table":    `{"columns":["a","b"],"rows":[[1]]}`,     // short row
		"nope":     `{}`,                                     // unknown type
		"callout":  `{"tone":"loud","text":"x"}`,             // bad enum
		"board":    `not json`,                               // not JSON
		"cards":    `{"items":[{"title":"x"}]} {"items":[]}`, // two objects
		"form":     `{"title":"forms are not blocks"}`,       // not a block type
		"timeline": `{"items":[]}`,                           // empty
	} {
		if _, err := renderBlock(kind, []byte(body)); err == nil {
			t.Errorf("an invalid hive:%s block was accepted: %s", kind, body)
		}
	}
	if got := preview("Intro\n\n" + hiveBlock("chart", `{"x":["a"],"series":[{"data":[1]}]}`) + "\n\nOutro"); strings.Contains(got, "series") || !strings.Contains(got, "Outro") {
		t.Errorf("preview shows a block's JSON: %q", got)
	}
}

func TestFetcherRefusesPrivateAddresses(t *testing.T) {
	for addr, want := range map[string]bool{
		"1.1.1.1": true, "2606:4700:4700::1111": true,
		"127.0.0.1": false, "10.0.0.5": false, "192.168.1.1": false, "172.16.0.1": false, "169.254.169.254": false,
		"100.64.0.1": false, "0.0.0.0": false, "::1": false, "fe80::1": false, "fd00::1": false, "::ffff:127.0.0.1": false, "224.0.0.1": false,
	} {
		if got := publicAddr(netip.MustParseAddr(addr)); got != want {
			t.Errorf("publicAddr(%s) = %v, want %v", addr, got, want)
		}
	}
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "secret") }))
	defer local.Close()
	f := newFetcher(NewHive(nil, Config{}, nil, nil).now)
	f.allowPrivate = true
	u, _ := f.parseFetchURL(local.URL)
	f.allowPrivate = false
	if res, err := f.get(context.Background(), u, "*/*", 1000); err == nil {
		t.Fatalf("the fetcher read a loopback server: %q", res.Body)
	}
	for _, raw := range []string{"http://example.com/", "file:///etc/passwd", "https://user:pw@example.com/", "//example.com", "javascript:alert(1)"} {
		if _, e := f.parseFetchURL(raw); e == nil {
			t.Errorf("parseFetchURL accepted %q", raw)
		}
	}
}

func testBlocksAreValidatedOnWrite(t *testing.T, e *testEnv) {
	key := e.enroll("scout@johnnycode.ai", "scout")
	var fail Error
	bad := "Numbers:\n\n" + hiveBlock("chart", `{"x":["a"],"series":[{"data":[1,2]}]}`)
	if s := e.call("POST", "/api/v1/threads", key, ThreadInput{Title: "t", Body: bad}, &fail); s != 400 || !strings.Contains(fail.Hint, "example:") {
		t.Fatalf("invalid block: status %d, error %+v; want 400 with an example in the hint", s, fail)
	}
	var thread Thread
	good := "Numbers:\n\n" + hiveBlock("metrics", `{"items":[{"label":"Tests","value":52}]}`)
	if s := e.call("POST", "/api/v1/threads", key, ThreadInput{Title: "t", Body: good}, &thread); s != 201 || thread.HasView || thread.Form != nil {
		t.Fatalf("valid block: status %d, %+v", s, thread)
	}
	if s := e.call("POST", "/api/v1/threads/"+thread.ID+"/replies", key, map[string]string{"body": hiveBlock("nope", `{}`)}, nil); s != 400 {
		t.Fatalf("reply with an unknown block type: status %d, want 400", s)
	}
	if s := e.call("PUT", "/api/v1/threads/"+thread.ID, key, map[string]string{"body": bad}, nil); s != 400 {
		t.Fatalf("editing in an invalid block: status %d, want 400", s)
	}
	// An ordinary code block is not a hive block and is left alone.
	if s := e.call("POST", "/api/v1/threads/"+thread.ID+"/replies", key, map[string]string{"body": fence + "json\n{not json}\n" + fence}, nil); s != 201 {
		t.Fatalf("reply with an ordinary code block: status %d", s)
	}
}

func testViewsAreSandboxedAndSelfContained(t *testing.T, e *testEnv) {
	e.useHTTP()
	hits := map[string]int{}
	outside := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits[r.URL.Path]++
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Errorf("the hive sent credentials to an outside host: %v", r.Header)
		}
		switch r.URL.Path {
		case "/npm/tiny@1.2.3":
			w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
			io.WriteString(w, `window.tiny = "</script><script>escaped()";`)
		case "/npm/sheet@1.0.0/dist/a.css":
			w.Header().Set("Content-Type", "text/css")
			io.WriteString(w, `.x{color:red}`)
		case "/npm/page@1.0.0":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<html>`)
		case "/pic.png":
			w.Header().Set("Content-Type", "image/png")
			io.WriteString(w, "\x89PNG fake")
		case "/data.json":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"stars":7}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer outside.Close()
	e.hive.fetch.allowPrivate = true
	e.hive.cfg.LibraryCDN = outside.URL + "/npm"
	host := strings.TrimPrefix(outside.URL, "http://")
	pic := outside.URL + "/pic.png"

	key := e.enroll("scout@johnnycode.ai", "scout")
	view := func(v ViewInput) map[string]any {
		return map[string]any{"title": "Dashboard", "body": "The view shows seven stars.", "view": v}
	}
	for name, v := range map[string]ViewInput{
		"empty html":          {HTML: "  "},
		"unpinned library":    {HTML: "x", Libraries: []string{"tiny@latest"}},
		"library path escape": {HTML: "x", Libraries: []string{"tiny@1.2.3/../../etc"}},
		"missing library":     {HTML: "x", Libraries: []string{"gone@9.9.9"}},
		"library is html":     {HTML: "x", Libraries: []string{"page@1.0.0"}},
		"image is not https":  {HTML: "x", Images: []string{"ftp://example.com/a.png"}},
		"image is not image":  {HTML: "x", Images: []string{outside.URL + "/data.json"}},
		"too many hosts":      {HTML: "x", Hosts: strings.Split(strings.Repeat("a.example.com,", 11), ",")[:11]},
	} {
		if s := e.call("POST", "/api/v1/threads", key, view(v), nil); s != 400 {
			t.Errorf("%s: status %d, want 400", name, s)
		}
	}

	var thread Thread
	html := `<img src="` + pic + `"><div id="app"></div><script>app.textContent = tiny</script>`
	in := ViewInput{HTML: html, Libraries: []string{"tiny@1.2.3", "sheet@1.0.0/dist/a.css"}, Images: []string{pic}, Hosts: []string{host}}
	if s := e.call("POST", "/api/v1/threads", key, view(in), &thread); s != 201 || !thread.HasView {
		t.Fatalf("create with view: status %d, %+v", s, thread)
	}
	// The same library again is served from the hive's copy.
	if s := e.call("POST", "/api/v1/threads", key, view(ViewInput{HTML: "y", Libraries: []string{"tiny@1.2.3"}}), nil); s != 201 || hits["/npm/tiny@1.2.3"] != 1 {
		t.Fatalf("second use of a library: status %d, fetched %d times; want it fetched once", s, hits["/npm/tiny@1.2.3"])
	}

	// Agents get the view's parts, and its HTML only on request.
	var got Thread
	e.call("GET", "/api/v1/threads/"+thread.ID, key, nil, &got)
	if got.View == nil || got.View.HTML != "" || got.View.HTMLBytes != len(html) || len(got.View.Libraries) != 2 || len(got.View.Hosts) != 1 || got.View.Hosts[0].Status != "unreviewed" {
		t.Fatalf("thread view = %+v", got.View)
	}
	e.call("GET", "/api/v1/threads/"+thread.ID+"?view=full", key, nil, &got)
	if got.View.HTML != html {
		t.Fatalf("view=full did not return the HTML")
	}

	// A person sees it in a frame whose document can reach nothing.
	person := e.browser("reader@johnnycode.ai")
	status, hd, page := e.do(person, "GET", "/dashboard/threads/"+thread.ID, nil)
	if status != 200 || !strings.Contains(page, `class="hive-view canvas-frame"`) || !strings.Contains(page, `sandbox="allow-scripts`) || strings.Contains(page, "app.textContent") {
		t.Fatalf("thread page: status %d; want a sandboxed frame and none of the view's own markup in the page", status)
	}
	if csp := hd.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src "+e.srv.URL+"/static/;") || !strings.Contains(csp, "frame-src 'self'") || strings.Contains(csp, "unsafe-inline") {
		t.Errorf("thread page CSP = %q; scripts must come only from /static/", csp)
	}
	status, hd, doc := e.do(person, "GET", "/dashboard/threads/"+thread.ID+"/view", nil, "Sec-Fetch-Dest", "iframe")
	csp := hd.Get("Content-Security-Policy")
	if status != 200 || hd.Get("X-Frame-Options") != "" {
		t.Fatalf("view frame: status %d, X-Frame-Options %q", status, hd.Get("X-Frame-Options"))
	}
	for _, want := range []string{"default-src 'none'", "connect-src 'none'", "sandbox allow-scripts", "frame-ancestors 'self'", "form-action 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("view frame CSP lacks %q: %s", want, csp)
		}
	}
	if strings.Contains(csp, "http") || strings.Contains(csp, "allow-same-origin") || strings.Contains(csp, "allow-top-navigation") || strings.Contains(csp, "allow-forms") {
		t.Errorf("view frame CSP lets the frame reach something: %s", csp)
	}
	for _, want := range []string{`window.tiny = "<\/script><script>escaped()";`, ".x{color:red}", `<img src="data:image/png;base64,`, "window.hive", `"title":"Dashboard"`} {
		if !strings.Contains(doc, want) {
			t.Errorf("view document lacks %q", want)
		}
	}
	if strings.Contains(doc, outside.URL) && strings.Contains(doc, `src="`+outside.URL) {
		t.Error("view document still points at the outside host")
	}
	// A hive that another site may frame lets that site be above the view too.
	embedded := e.hive.cfg
	embedded.FrameAncestors = "https://portal.example"
	if got := NewServer(embedded, e.hive).frameCSP(); !strings.Contains(got, "frame-ancestors 'self' https://portal.example;") {
		t.Errorf("with FRAME_ANCESTORS set, the view frame's CSP is %q", got)
	}
	// Opened as a page of its own it is not shown; signed out, not at all.
	if s, h, _ := e.do(person, "GET", "/dashboard/threads/"+thread.ID+"/view", nil, "Sec-Fetch-Dest", "document"); s != http.StatusSeeOther || !strings.HasSuffix(h.Get("Location"), "/dashboard/threads/"+thread.ID) {
		t.Errorf("view opened as a top-level page: status %d", s)
	}
	anon := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if s, _, _ := e.do(anon, "GET", "/dashboard/threads/"+thread.ID+"/view", nil, "Sec-Fetch-Dest", "iframe"); s != http.StatusSeeOther {
		t.Errorf("view frame without a session: status %d, want a redirect to sign in", s)
	}

	// Live data: only declared hosts, through the hive, under the operator's rules.
	data := "/dashboard/threads/" + thread.ID + "/data"
	ask := func(c *http.Client, target string) (int, string) {
		body, _ := json.Marshal(map[string]string{"url": target})
		s, _, out := e.do(c, "POST", data, string(body))
		return s, out
	}
	if s, out := ask(person, outside.URL+"/data.json"); s != 200 || !strings.Contains(out, `{\"stars\":7}`) {
		t.Fatalf("live data from a declared host: status %d, %s", s, out)
	}
	ask(person, outside.URL+"/data.json")
	if hits["/data.json"] != 2 { // once as a refused image, once as data; the repeat came from the cache
		t.Errorf("live data was fetched %d times; a repeat within a minute should come from the cache", hits["/data.json"])
	}
	if s, _ := ask(person, "http://undeclared.example.com/x"); s != 403 {
		t.Errorf("live data from an undeclared host: status %d, want 403", s)
	}
	if s, _ := ask(anon, outside.URL+"/data.json"); s != 401 {
		t.Errorf("live data without a session: status %d, want 401", s)
	}
	admin := e.browser("owner@johnnycode.ai")
	rule := func(c *http.Client, status string) int {
		s, _, _ := e.do(c, "POST", "/admin/hosts", url.Values{"host": {host}, "status": {status}})
		return s
	}
	if s := rule(person, "blocked"); s != 404 {
		t.Errorf("a non-admin ruled on a host: status %d, want 404", s)
	}
	if s := rule(admin, "blocked"); s != http.StatusSeeOther {
		t.Fatalf("admin blocks a host: status %d", s)
	}
	if s, _ := ask(person, outside.URL+"/data.json"); s != 403 {
		t.Errorf("live data from a blocked host: status %d, want 403", s)
	}
	rule(admin, "unreviewed")
	e.hive.cfg.LiveData = "approved"
	if s, _ := ask(person, outside.URL+"/data.json"); s != 403 {
		t.Errorf("LIVE_DATA=approved, host not approved: status %d, want 403", s)
	}
	rule(admin, "approved")
	if s, _ := ask(person, outside.URL+"/data.json"); s != 200 {
		t.Errorf("LIVE_DATA=approved, host approved: status %d, want 200", s)
	}
	if _, _, page := e.do(admin, "GET", "/admin", nil); !strings.Contains(page, host) {
		t.Error("the admin page does not list the host a view declared")
	}
	e.hive.cfg.LiveData = "off"
	if s, _ := ask(person, outside.URL+"/data.json"); s != 403 {
		t.Errorf("LIVE_DATA=off: status %d, want 403", s)
	}
	if s := e.call("POST", "/api/v1/threads", key, view(ViewInput{HTML: "x", Hosts: []string{host}}), nil); s != 400 {
		t.Errorf("LIVE_DATA=off, view with hosts: status %d, want 400", s)
	}

	// null removes the view; only the author may.
	other := e.enroll("teammate@johnnycode.ai", "teammate")
	if s := e.call("PUT", "/api/v1/threads/"+thread.ID, other, json.RawMessage(`{"view":null}`), nil); s != 403 {
		t.Errorf("another agent removed the view: status %d", s)
	}
	if s := e.call("PUT", "/api/v1/threads/"+thread.ID, key, json.RawMessage(`{"view":null}`), &got); s != 200 || got.HasView {
		t.Errorf("removing the view: status %d, has_view %v", s, got.HasView)
	}
}

func testFormsCollectAnswersFromAdmins(t *testing.T, e *testEnv) {
	e.useHTTP()
	key := e.enroll("scout@johnnycode.ai", "scout")
	form := func(fields string) json.RawMessage {
		return json.RawMessage(`{"title":"Ship?","body":"Asking whether to ship.","form":{"title":"Ship the preview?","fields":` + fields + `}}`)
	}
	for name, fields := range map[string]string{
		"no fields":         `[]`,
		"duplicate id":      `[{"id":"a","label":"A","type":"text"},{"id":"a","label":"B","type":"text"}]`,
		"bad id":            `[{"id":"Not OK","label":"A","type":"text"}]`,
		"unknown type":      `[{"id":"a","label":"A","type":"password"}]`,
		"choice no options": `[{"id":"a","label":"A","type":"choice"}]`,
		"options on text":   `[{"id":"a","label":"A","type":"text","options":["x","y"]}]`,
		"min above max":     `[{"id":"a","label":"A","type":"number","min":5,"max":1}]`,
	} {
		if s := e.call("POST", "/api/v1/threads", key, form(fields), nil); s != 400 {
			t.Errorf("form with %s: status %d, want 400", name, s)
		}
	}
	var thread Thread
	fields := `[{"id":"decision","label":"Decision","type":"choice","options":["Ship","Wait"],"required":true},
		{"id":"areas","label":"Areas","type":"multi","options":["api","ui"]},
		{"id":"score","label":"Score","type":"number","min":1,"max":5},
		{"id":"urgent","label":"Urgent","type":"boolean"},
		{"id":"notes","label":"Notes","type":"text"}]`
	if s := e.call("POST", "/api/v1/threads", key, form(fields), &thread); s != 201 || thread.Form == nil || len(thread.Form.Fields) != 5 {
		t.Fatalf("create with form: status %d, %+v", s, thread)
	}
	var active ActiveContext
	e.call("GET", "/api/v1/context/active", key, nil, &active)
	if len(active.AwaitingResponse) != 1 {
		t.Fatalf("awaiting_response has %d threads, want 1", len(active.AwaitingResponse))
	}

	respond := "/dashboard/threads/" + thread.ID + "/respond"
	reader, admin := e.browser("reader@johnnycode.ai"), e.browser("owner@johnnycode.ai")
	if _, _, page := e.do(reader, "GET", "/dashboard/threads/"+thread.ID, nil); !strings.Contains(page, "Ship the preview?") || strings.Contains(page, `action="`+respond) {
		t.Error("a non-admin should see the question but no form to submit")
	}
	if _, _, page := e.do(admin, "GET", "/dashboard/threads/"+thread.ID, nil); !strings.Contains(page, `action="`+respond) || !strings.Contains(page, `name="decision"`) {
		t.Error("an admin should get the form")
	}
	ok := url.Values{"decision": {"Ship"}, "areas": {"api", "ui"}, "score": {"4"}, "urgent": {"true"}, "notes": {"Go."}}
	if s, _, _ := e.do(reader, "POST", respond, ok); s != 403 {
		t.Errorf("a non-admin answered: status %d, want 403", s)
	}
	for name, v := range map[string]url.Values{
		"missing required": {"notes": {"x"}},
		"unknown option":   {"decision": {"Maybe"}},
		"unknown multi":    {"decision": {"Ship"}, "areas": {"db"}},
		"out of range":     {"decision": {"Ship"}, "score": {"9"}},
		"not a number":     {"decision": {"Ship"}, "score": {"lots"}},
	} {
		if s, _, _ := e.do(admin, "POST", respond, v); s != 400 {
			t.Errorf("answer with %s: status %d, want 400", name, s)
		}
	}
	// Browsers mark cross-site posts; those are refused before anything is recorded.
	if s, _, _ := e.do(admin, "POST", respond, ok, "Sec-Fetch-Site", "cross-site"); s != 403 {
		t.Errorf("cross-site answer: status %d, want 403", s)
	}
	if s, _, _ := e.do(admin, "POST", respond, ok); s != http.StatusSeeOther {
		t.Fatalf("admin answers: status %d", s)
	}

	var responses []Response
	if s := e.call("GET", "/api/v1/threads/"+thread.ID+"/responses", key, nil, &responses); s != 200 || len(responses) != 1 {
		t.Fatalf("responses: status %d, %d of them", s, len(responses))
	}
	r := responses[0]
	if r.Responder != "owner@johnnycode.ai" || r.Values["decision"] != "Ship" || r.Values["score"] != 4.0 || r.Values["urgent"] != true || len(r.Values["areas"].([]any)) != 2 {
		t.Fatalf("response = %+v", r)
	}
	var got Thread
	e.call("GET", "/api/v1/threads/"+thread.ID, key, nil, &got)
	if got.ResponseCount != 1 || len(got.Responses) != 1 || !got.UpdatedAt.After(thread.UpdatedAt) {
		t.Fatalf("thread after an answer: count %d, %d responses, updated %v -> %v", got.ResponseCount, len(got.Responses), thread.UpdatedAt, got.UpdatedAt)
	}
	e.call("GET", "/api/v1/context/active", key, nil, &active)
	if len(active.AwaitingResponse) != 0 {
		t.Error("an answered thread is still listed as awaiting_response")
	}
	if _, _, page := e.do(reader, "GET", "/dashboard/threads/"+thread.ID, nil); !strings.Contains(page, "owner@johnnycode.ai") || !strings.Contains(page, "Go.") {
		t.Error("the thread page does not show the answer")
	}
	if s := e.call("GET", "/api/v1/threads/"+thread.ID+"/responses", "", nil, nil); s != 401 {
		t.Errorf("responses without a key: status %d, want 401", s)
	}

	// MCP offers the same.
	var rpc struct {
		Result struct {
			Content []struct{ Text string }
		}
	}
	call := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "list_responses", "arguments": map[string]string{"thread_id": thread.ID}}}
	if s := e.call("POST", "/mcp", key, call, &rpc); s != 200 || len(rpc.Result.Content) != 1 || !strings.Contains(rpc.Result.Content[0].Text, `"decision":"Ship"`) {
		t.Fatalf("MCP list_responses: status %d, %+v", s, rpc)
	}
	create := map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "create_thread",
		"arguments": map[string]any{"title": "Via MCP", "body": hiveBlock("callout", `{"text":"hello"}`),
			"view": map[string]any{"html": "<p>hi</p>"}, "form": map[string]any{"title": "OK?", "fields": []map[string]any{{"id": "ok", "label": "OK", "type": "boolean"}}}}}}
	if s := e.call("POST", "/mcp", key, create, &rpc); s != 200 || !strings.Contains(rpc.Result.Content[0].Text, `"has_view":true`) || !strings.Contains(rpc.Result.Content[0].Text, `"form":{`) {
		t.Fatalf("MCP create_thread with view and form: status %d, %+v", s, rpc)
	}

	// Removing the form keeps the record of what was answered out of the way;
	// deleting the thread removes its answers.
	var cleared Thread
	if s := e.call("PUT", "/api/v1/threads/"+thread.ID, key, json.RawMessage(`{"form":null}`), &cleared); s != 200 || cleared.Form != nil {
		t.Errorf("removing the form: status %d", s)
	}
	if s, _, _ := e.do(admin, "POST", respond, ok); s != 404 {
		t.Errorf("answering a thread with no form: status %d, want 404", s)
	}
	if s := e.call("DELETE", "/api/v1/threads/"+thread.ID, key, nil, nil); s != 204 {
		t.Fatalf("delete thread with answers: status %d", s)
	}
	var n int
	e.hive.db.QueryRow(`SELECT COUNT(*) FROM responses`).Scan(&n)
	if n != 0 {
		t.Errorf("%d answers outlived their thread", n)
	}
}

func testGuideTeachesRichThreads(t *testing.T, e *testEnv) {
	srv := NewServer(e.hive.cfg, e.hive)
	guide := srv.llmsTxt()
	for _, want := range []string{"hive:chart", "hive:board", fence + "hive:callout", `"view": {"html"`, "hive.fetch(url)", `"form": {"title"`, "list_responses", "awaiting_response",
		"no house style", "hive/base@1", "hive/pm@1", "ticket.<id>", "sprint.<id>", "POST /api/v1/kits", "hive.data.onChange", "/data/{name}", `"card": {"html"`, "publish_kit", "set_thread_data"} {
		if !strings.Contains(guide, want) {
			t.Errorf("the agent guide does not mention %q", want)
		}
	}
	if strings.Contains(guide, "%!") {
		t.Errorf("the agent guide has a formatting error:\n%s", guide)
	}
	spec, _ := json.Marshal(srv.openAPI())
	for _, want := range []string{`"View"`, `"Form"`, "/api/v1/threads/{id}/responses"} {
		if !strings.Contains(string(spec), want) {
			t.Errorf("the OpenAPI document lacks %s", want)
		}
	}
	var info struct {
		Rich struct {
			Blocks   []string
			LiveData string `json:"live_data"`
		} `json:"rich_threads"`
	}
	if s := e.call("GET", "/api/v1", "", nil, &info); s != 200 || len(info.Rich.Blocks) != len(blockTypes) || info.Rich.LiveData != "declared" {
		t.Errorf("GET /api/v1 rich_threads = %+v", info.Rich)
	}
}
