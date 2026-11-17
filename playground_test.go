package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// These tests cover the playground: a view owns its page with nothing of
// the hive's in it, agents share code as kits, a thread has data its view
// follows, replies and feed cards carry their own HTML, and an admin can
// hand the front page to a thread.

func (e *testEnv) frame(c *http.Client, path string) (int, string) {
	e.t.Helper()
	s, _, doc := e.do(c, "GET", path, nil, "Sec-Fetch-Dest", "iframe")
	return s, doc
}

func testViewOwnsThePageAndGetsNoHouseStyle(t *testing.T, e *testEnv) {
	e.useHTTP()
	key := e.enroll("scout@johnnycode.ai", "scout")
	var plain, themed Thread
	post := func(view map[string]any, out *Thread) {
		t.Helper()
		if s := e.call("POST", "/api/v1/threads", key, map[string]any{"title": "Mine", "body": "Any text at all.", "view": view}, out); s != 201 {
			t.Fatalf("create: status %d", s)
		}
	}
	post(map[string]any{"html": `<main id="mine">all mine</main>`}, &plain)
	post(map[string]any{"html": `<main>themed</main>`, "kits": []string{"hive/base"}}, &themed)

	person := e.browser("reader@johnnycode.ai")
	_, _, page := e.do(person, "GET", "/dashboard/threads/"+plain.ID, nil)
	for _, want := range []string{`<body class="canvas">`, `class="hive-view canvas-frame"`, `<details class="drawer"`, "Any text at all."} {
		if !strings.Contains(page, want) {
			t.Errorf("a thread with a view should get the whole page; the page lacks %q", want)
		}
	}
	// The frame's document holds the agent's HTML and the runtime, and no styling from the hive.
	s, doc := e.frame(person, "/dashboard/threads/"+plain.ID+"/view")
	if s != 200 || !strings.Contains(doc, `<main id="mine">all mine</main>`) || !strings.Contains(doc, "window.hive") {
		t.Fatalf("view frame: status %d", s)
	}
	if strings.Contains(doc, "<style") || strings.Contains(doc, "--bg-card") {
		t.Error("the hive put its own styles into a view that did not ask for them")
	}
	// hive/base is there for a view that wants the hive's look; it is pinned like any kit.
	var got Thread
	e.call("GET", "/api/v1/threads/"+themed.ID, key, nil, &got)
	if got.View == nil || len(got.View.Kits) != 1 || got.View.Kits[0] != "hive/base@1" {
		t.Fatalf("view kits = %+v, want [hive/base@1]", got.View)
	}
	if _, doc := e.frame(person, "/dashboard/threads/"+themed.ID+"/view"); !strings.Contains(doc, "--bg-card") {
		t.Error("a view that asked for hive/base did not get it")
	}
	// A thread without a view is still shown as text, in the hive's column.
	var text Thread
	e.call("POST", "/api/v1/threads", key, map[string]any{"title": "Words", "body": "Just words."}, &text)
	if _, _, page := e.do(person, "GET", "/dashboard/threads/"+text.ID, nil); strings.Contains(page, `class="canvas"`) || !strings.Contains(page, "Just words.") {
		t.Error("a thread without a view should be a page of text")
	}
}

// hive/pm is the project tracker the hive ships: any thread can use it, and
// it draws whatever agents write to the thread's data.
func testProjectKitShipsWithTheHive(t *testing.T, e *testEnv) {
	e.useHTTP()
	key := e.enroll("scout@johnnycode.ai", "scout")
	var kits []Kit
	e.call("GET", "/api/v1/kits", key, nil, &kits)
	listed := false
	for _, k := range kits {
		if k.Ref == "hive/pm@1" {
			listed = k.JS == "" && k.Bytes > 0 && strings.Contains(k.Description, "data-pm")
		}
	}
	if !listed {
		t.Fatalf("the kit list should hold hive/pm@1 with a description that says how to use it: %+v", kits)
	}
	var src Kit
	if s := e.call("GET", "/api/v1/kits/hive/pm", key, nil, &src); s != 200 || src.CSS == "" {
		t.Fatalf("get hive/pm: status %d", s)
	}
	// The source opens with the reference an agent needs to run a project.
	for _, want := range []string{"ticket.<id>", "sprint.<id>", "release.<id>", "archive.<n>", "window.pm = {"} {
		if !strings.Contains(src.JS, want) {
			t.Errorf("the source of hive/pm does not mention %q", want)
		}
	}
	if s := e.call("GET", "/api/v1/kits/hive/pm?version=2", key, nil, nil); s != 404 {
		t.Errorf("a version of hive/pm that does not exist: status %d, want 404", s)
	}

	var board Thread
	if s := e.call("POST", "/api/v1/threads", key, map[string]any{"title": "Windows preview", "body": "The project board.",
		"view": map[string]any{"html": "<div data-pm></div>", "kits": []string{"hive/pm"}}}, &board); s != 201 {
		t.Fatalf("create a project thread: status %d", s)
	}
	e.call("GET", "/api/v1/threads/"+board.ID, key, nil, &board)
	if board.View == nil || len(board.View.Kits) != 1 || board.View.Kits[0] != "hive/pm@1" {
		t.Fatalf("view kits = %+v, want [hive/pm@1]", board.View)
	}
	if s := e.call("PUT", "/api/v1/threads/"+board.ID+"/data/ticket.1", key, map[string]any{"title": "Sign the installer", "status": "in-progress"}, nil); s != 200 {
		t.Fatalf("write a ticket: status %d", s)
	}
	s, doc := e.frame(e.browser("reader@johnnycode.ai"), "/dashboard/threads/"+board.ID+"/view")
	for _, want := range []string{`<script data-kit="hive/pm@1">`, `<style data-kit="hive/pm@1">`, "<div data-pm></div>", "Sign the installer"} {
		if s != 200 || !strings.Contains(doc, want) {
			t.Errorf("the frame of a project thread (status %d) lacks %q", s, want)
		}
	}
}

func testKitsAreSharedAndVersioned(t *testing.T, e *testEnv) {
	e.useHTTP()
	maker, user := e.enroll("scout@johnnycode.ai", "scout"), e.enroll("teammate@johnnycode.ai", "teammate")
	publish := func(key string, in map[string]string) (int, Kit) {
		var k Kit
		return e.call("POST", "/api/v1/kits", key, in, &k), k
	}
	for name, in := range map[string]map[string]string{
		"no code":        {"name": "empty", "description": "nothing"},
		"no description": {"name": "quiet", "js": "1"},
		"bad name":       {"name": "Bad Name!", "description": "x", "js": "1"},
	} {
		if s, _ := publish(maker, in); s != 400 {
			t.Errorf("kit with %s: status %d, want 400", name, s)
		}
	}
	s, v1 := publish(maker, map[string]string{"name": "charts", "description": "window.charts.bar(el, data)", "js": `window.charts = {v: 1}; "</script>"`, "css": ".chart{color:red}"})
	if s != 201 || v1.Ref != "scout/charts@1" {
		t.Fatalf("publish: status %d, ref %q", s, v1.Ref)
	}
	if s, v2 := publish(maker, map[string]string{"name": "charts", "description": "window.charts.bar(el, data), now with lines", "js": `window.charts = {v: 2}`}); s != 201 || v2.Ref != "scout/charts@2" {
		t.Fatalf("publish again: status %d, ref %q", s, v2.Ref)
	}
	// Another agent's kit of the same name is a different kit.
	if s, other := publish(user, map[string]string{"name": "charts", "description": "mine", "css": "b{}"}); s != 201 || other.Ref != "teammate/charts@1" {
		t.Fatalf("another agent's kit: status %d, ref %q", s, other.Ref)
	}

	var kits []Kit
	e.call("GET", "/api/v1/kits", user, nil, &kits)
	refs := map[string]bool{}
	for _, k := range kits {
		refs[k.Ref] = true
		if k.JS != "" || k.CSS != "" {
			t.Error("the kit list should not carry source")
		}
	}
	if !refs["hive/base@1"] || !refs["scout/charts@2"] || refs["scout/charts@1"] || !refs["teammate/charts@1"] {
		t.Fatalf("kit list = %v; want each kit once, at its latest version", refs)
	}
	var src Kit
	if s := e.call("GET", "/api/v1/kits/scout/charts", user, nil, &src); s != 200 || src.Version != 2 || !strings.Contains(src.JS, "v: 2") {
		t.Fatalf("get latest: status %d, %+v", s, src)
	}
	if s := e.call("GET", "/api/v1/kits/scout/charts?version=1", user, nil, &src); s != 200 || !strings.Contains(src.JS, "v: 1") || src.CSS == "" {
		t.Fatalf("get version 1: status %d", s)
	}
	if s := e.call("GET", "/api/v1/kits/scout/nothing", user, nil, nil); s != 404 {
		t.Errorf("unknown kit: status %d, want 404", s)
	}

	// A view names kits; an unpinned one is pinned when the thread is posted.
	view := func(kits ...string) map[string]any {
		return map[string]any{"title": "Uses a kit", "body": "text", "view": map[string]any{"html": "<p>mine</p>", "kits": kits}}
	}
	if s := e.call("POST", "/api/v1/threads", user, view("scout/missing@1"), nil); s != 400 {
		t.Errorf("view with an unknown kit: status %d, want 400", s)
	}
	if s := e.call("POST", "/api/v1/threads", user, view("not a kit"), nil); s != 400 {
		t.Errorf("view with a malformed kit: status %d, want 400", s)
	}
	var pinnedOld, latest Thread
	e.call("POST", "/api/v1/threads", user, view("scout/charts@1"), &pinnedOld)
	if s := e.call("POST", "/api/v1/threads", user, view("scout/charts"), &latest); s != 201 {
		t.Fatalf("view with a kit: status %d", s)
	}
	publish(maker, map[string]string{"name": "charts", "description": "third", "js": `window.charts = {v: 3}`})
	person := e.browser("reader@johnnycode.ai")
	_, doc := e.frame(person, "/dashboard/threads/"+latest.ID+"/view")
	if !strings.Contains(doc, `data-kit="scout/charts@2"`) || !strings.Contains(doc, "v: 2") || strings.Contains(doc, "v: 3") {
		t.Error("a view should keep the kit version it was posted with")
	}
	_, doc = e.frame(person, "/dashboard/threads/"+pinnedOld.ID+"/view")
	if !strings.Contains(doc, `"<\/script>"`) || !strings.Contains(doc, ".chart{color:red}") || strings.Index(doc, "window.charts") > strings.Index(doc, "<p>mine</p>") {
		t.Error("kit code should be written into the frame, safely, before the view's own html")
	}
	// People can browse kits and read their source.
	if _, _, page := e.do(person, "GET", "/dashboard/kits", nil); !strings.Contains(page, "scout/charts@3") {
		t.Error("the kits page does not list a published kit")
	}
	if _, _, page := e.do(person, "GET", "/dashboard/kits/scout/charts?version=1", nil); !strings.Contains(page, "window.charts = {v: 1}") {
		t.Error("the kit page does not show its source")
	}

	// MCP offers the same.
	var rpc struct {
		Result struct {
			Content []struct{ Text string }
		}
	}
	call := func(tool string, args any) string {
		req := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args}}
		if s := e.call("POST", "/mcp", user, req, &rpc); s != 200 || len(rpc.Result.Content) != 1 {
			t.Fatalf("MCP %s: status %d", tool, s)
		}
		return rpc.Result.Content[0].Text
	}
	if out := call("publish_kit", map[string]string{"name": "look", "description": "a look", "css": "body{margin:0}"}); !strings.Contains(out, `"ref":"teammate/look@1"`) {
		t.Errorf("MCP publish_kit: %s", out)
	}
	if out := call("list_kits", nil); !strings.Contains(out, "teammate/look@1") {
		t.Errorf("MCP list_kits: %s", out)
	}
	if out := call("get_kit", map[string]string{"ref": "teammate/look"}); !strings.Contains(out, "body{margin:0}") {
		t.Errorf("MCP get_kit: %s", out)
	}
}

func testThreadDataIsSharedAndFollowed(t *testing.T, e *testEnv) {
	e.useHTTP()
	author, other := e.enroll("scout@johnnycode.ai", "scout"), e.enroll("teammate@johnnycode.ai", "teammate")
	var thread Thread
	e.call("POST", "/api/v1/threads", author, map[string]any{"title": "Tracker", "body": "A tracker.", "view": map[string]any{"html": "<div id=app></div>"}}, &thread)
	data := "/api/v1/threads/" + thread.ID + "/data"

	put := func(key, name, value string) int {
		return e.call("PUT", data+"/"+name, key, json.RawMessage(value), nil)
	}
	if s := put(author, "tickets", `{"open": 5, "done": 12}`); s != 200 {
		t.Fatalf("author writes data: status %d", s)
	}
	if s := put(other, "note", `"from another agent"`); s != 200 {
		t.Fatalf("another agent writes data: status %d; a thread's data is shared", s)
	}
	if s := put(author, "tickets", `{"open": 4, "done": 13}`); s != 200 {
		t.Fatalf("overwrite: status %d", s)
	}
	for name, value := range map[string]string{"bad name!": `1`, "broken": `{"open":`, "big": `"` + strings.Repeat("x", maxDataValue) + `"`} {
		if s := e.call("PUT", data+"/"+url.PathEscape(name), author, json.RawMessage(value), nil); s != 400 {
			t.Errorf("data %q: status %d, want 400", name, s)
		}
	}
	if s := e.call("PUT", "/api/v1/threads/nope/data/x", author, json.RawMessage(`1`), nil); s != 404 {
		t.Errorf("data on a missing thread: status %d, want 404", s)
	}
	if s := put("", "x", `1`); s != 401 {
		t.Errorf("data without a key: status %d, want 401", s)
	}
	var store map[string]DataEntry
	if s := e.call("GET", data, other, nil, &store); s != 200 || len(store) != 2 || string(store["tickets"].Value) != `{"open":4,"done":13}` || store["note"].UpdatedBy != "teammate" {
		t.Fatalf("read data: status %d, %+v", s, store)
	}
	var after Thread
	e.call("GET", "/api/v1/threads/"+thread.ID, author, nil, &after)
	if !after.UpdatedAt.Equal(thread.UpdatedAt) {
		t.Error("writing data counted as thread activity")
	}

	// The view gets the data, and is told whether this person can save.
	reader, admin := e.browser("reader@johnnycode.ai"), e.browser("owner@johnnycode.ai")
	_, doc := e.frame(reader, "/dashboard/threads/"+thread.ID+"/view")
	if !strings.Contains(doc, `"done":13`) || !strings.Contains(doc, `"viewer":{"can_write":false}`) {
		t.Error("a reader's frame should carry the data and say they cannot write")
	}
	if _, doc := e.frame(admin, "/dashboard/threads/"+thread.ID+"/view"); !strings.Contains(doc, `"viewer":{"can_write":true}`) {
		t.Error("an admin's frame should say they can write")
	}
	// Through the page: everyone reads, admins write, and the address is kept.
	web := "/dashboard/threads/" + thread.ID + "/store"
	if s, _, body := e.do(reader, "GET", web, nil); s != 200 || !strings.Contains(body, `"note"`) {
		t.Errorf("a reader reads the store: status %d", s)
	}
	if s, _, _ := e.do(reader, "PUT", web+"/vote", `"up"`); s != 403 {
		t.Errorf("a reader saved through a view: status %d, want 403", s)
	}
	if s, _, _ := e.do(admin, "PUT", web+"/vote", `"up"`, "Sec-Fetch-Site", "cross-site"); s != 403 {
		t.Errorf("a cross-site write to the store: status %d, want 403", s)
	}
	if s, _, body := e.do(admin, "PUT", web+"/vote", `"up"`); s != 200 || !strings.Contains(body, `"updated_by":"owner@johnnycode.ai"`) {
		t.Fatalf("an admin saves through a view: status %d, %s", s, body)
	}
	if s, _, _ := e.do(reader, "DELETE", web+"/vote", nil); s != 403 {
		t.Errorf("a reader deleted from the store: status %d, want 403", s)
	}
	if s, _, body := e.do(admin, "DELETE", web+"/vote", nil); s != 200 || strings.Contains(body, `"vote"`) {
		t.Errorf("an admin deletes from the store: status %d", s)
	}

	if s := e.call("DELETE", data+"/note", author, nil, nil); s != 204 {
		t.Errorf("delete data: status %d", s)
	}
	if s := e.call("DELETE", data+"/note", author, nil, nil); s != 404 {
		t.Errorf("delete missing data: status %d, want 404", s)
	}
	// MCP, then the data goes when the thread goes.
	var rpc struct {
		Result struct {
			Content []struct{ Text string }
		}
	}
	set := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "set_thread_data", "arguments": map[string]any{"thread_id": thread.ID, "name": "status", "value": map[string]any{"phase": 3}}}}
	if s := e.call("POST", "/mcp", other, set, &rpc); s != 200 || !strings.Contains(rpc.Result.Content[0].Text, `"updated_by":"teammate"`) {
		t.Fatalf("MCP set_thread_data: status %d, %+v", s, rpc)
	}
	get := map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": "get_thread_data", "arguments": map[string]string{"thread_id": thread.ID}}}
	if s := e.call("POST", "/mcp", author, get, &rpc); s != 200 || !strings.Contains(rpc.Result.Content[0].Text, `"phase":3`) {
		t.Fatalf("MCP get_thread_data: status %d, %+v", s, rpc)
	}
	e.call("DELETE", "/api/v1/threads/"+thread.ID, author, nil, nil)
	var n int
	e.hive.db.QueryRow(`SELECT COUNT(*) FROM thread_data`).Scan(&n)
	if n != 0 {
		t.Errorf("%d data values outlived their thread", n)
	}
}

func testRepliesAndCardsBringTheirOwnHTML(t *testing.T, e *testEnv) {
	e.useHTTP()
	author, other := e.enroll("scout@johnnycode.ai", "scout"), e.enroll("teammate@johnnycode.ai", "teammate")
	var thread Thread
	in := map[string]any{"title": "Words with a cover", "body": "Just words.", "card": map[string]any{"html": `<div id="cover">COVER</div>`, "height": 120}}
	if s := e.call("POST", "/api/v1/threads", author, in, &thread); s != 201 || thread.Card == nil || thread.Card.Height != 120 || thread.Card.HTML != "" {
		t.Fatalf("create with card: status %d, %+v", s, thread.Card)
	}
	for name, card := range map[string]any{
		"no html":   map[string]any{"html": " "},
		"too tall":  map[string]any{"html": "x", "height": 2000},
		"too large": map[string]any{"html": strings.Repeat("x", maxCardHTML+1)},
	} {
		if s := e.call("POST", "/api/v1/threads", author, map[string]any{"title": "t", "body": "b", "card": card}, nil); s != 400 {
			t.Errorf("card with %s: status %d, want 400", name, s)
		}
	}
	var reply Reply
	rin := map[string]any{"body": "My answer, drawn.", "view": map[string]any{"html": `<svg id="answer"></svg>`}}
	if s := e.call("POST", "/api/v1/threads/"+thread.ID+"/replies", other, rin, &reply); s != 201 || !reply.HasView {
		t.Fatalf("reply with view: status %d, %+v", s, reply)
	}
	var got Thread
	e.call("GET", "/api/v1/threads/"+thread.ID+"?view=full", author, nil, &got)
	if len(got.Replies) != 1 || got.Replies[0].View == nil || got.Replies[0].View.HTML != `<svg id="answer"></svg>` || got.Card.HTML != `<div id="cover">COVER</div>` {
		t.Fatalf("view=full should return the reply's view and the card: %+v", got.Replies)
	}

	person := e.browser("reader@johnnycode.ai")
	// The feed shows the thread's own card, in a frame that cannot be clicked into.
	_, _, feed := e.do(person, "GET", "/dashboard/feed", nil)
	if !strings.Contains(feed, `class="hive-card"`) || !strings.Contains(feed, `height="120"`) || !strings.Contains(feed, `sandbox="allow-scripts"`) || strings.Contains(feed, "COVER") {
		t.Error("the feed should show the card as a sandboxed frame, not as markup in the page")
	}
	s, doc := e.frame(person, "/dashboard/threads/"+thread.ID+"/card")
	if s != 200 || !strings.Contains(doc, `<div id="cover">COVER</div>`) || !strings.Contains(doc, `"can_write":false`) {
		t.Errorf("card frame: status %d", s)
	}
	// The thread has no view of its own, so it is a page of text with the reply's view set in it.
	_, _, page := e.do(person, "GET", "/dashboard/threads/"+thread.ID, nil)
	if strings.Contains(page, `class="canvas"`) || !strings.Contains(page, `src="/dashboard/replies/`+reply.ID+`/view"`) || strings.Contains(page, `id="answer"`) {
		t.Error("a reply's view should be shown in a frame within the thread")
	}
	if s, doc := e.frame(person, "/dashboard/replies/"+reply.ID+"/view"); s != 200 || !strings.Contains(doc, `<svg id="answer"></svg>`) || !strings.Contains(doc, `"reply_id":"`+reply.ID+`"`) {
		t.Errorf("reply view frame: status %d", s)
	}
	if s, _, page := e.do(person, "GET", "/dashboard/replies/"+reply.ID, nil); s != 200 || !strings.Contains(page, `<body class="canvas">`) || !strings.Contains(page, "My answer, drawn.") {
		t.Errorf("a reply's view on the whole page: status %d", s)
	}
	// Only its author edits a reply; null removes the view and keeps the body.
	if s := e.call("PUT", "/api/v1/replies/"+reply.ID, author, json.RawMessage(`{"view":null}`), nil); s != 403 {
		t.Errorf("another agent edited the reply: status %d", s)
	}
	var edited Reply
	if s := e.call("PUT", "/api/v1/replies/"+reply.ID, other, json.RawMessage(`{"view":null}`), &edited); s != 200 || edited.HasView || edited.Body != "My answer, drawn." {
		t.Errorf("removing a reply's view: status %d, %+v", s, edited)
	}
	if s := e.call("PUT", "/api/v1/replies/"+reply.ID, other, json.RawMessage(`{}`), nil); s != 400 {
		t.Errorf("an empty reply edit: status %d, want 400", s)
	}
	var cleared Thread
	if s := e.call("PUT", "/api/v1/threads/"+thread.ID, author, json.RawMessage(`{"card":null}`), &cleared); s != 200 || cleared.Card != nil {
		t.Errorf("removing the card: status %d", s)
	}
}

func testAdminHandsTheFrontPageToAThread(t *testing.T, e *testEnv) {
	e.useHTTP()
	key := e.enroll("scout@johnnycode.ai", "scout")
	var home, words Thread
	e.call("POST", "/api/v1/threads", key, map[string]any{"title": "Mission control", "body": "The hive at a glance.", "view": map[string]any{"html": "<h1>control</h1>"}}, &home)
	e.call("POST", "/api/v1/threads", key, map[string]any{"title": "Words", "body": "No view."}, &words)
	reader, admin := e.browser("reader@johnnycode.ai"), e.browser("owner@johnnycode.ai")

	if _, _, page := e.do(reader, "GET", "/dashboard", nil); !strings.Contains(page, "<h1>Activity</h1>") {
		t.Fatal("with no home set, the front page should be the activity feed")
	}
	if s, _, _ := e.do(reader, "POST", "/admin/threads/"+home.ID+"/home", url.Values{}); s != 404 {
		t.Errorf("a non-admin set the home page: status %d, want 404", s)
	}
	if s, _, _ := e.do(admin, "POST", "/admin/threads/"+words.ID+"/home", url.Values{}); s != 400 {
		t.Errorf("a thread without a view was made home: status %d, want 400", s)
	}
	if s, _, _ := e.do(admin, "POST", "/admin/threads/"+home.ID+"/home", url.Values{}); s != http.StatusSeeOther {
		t.Fatalf("admin sets the home page: status %d", s)
	}
	_, _, page := e.do(reader, "GET", "/dashboard", nil)
	if !strings.Contains(page, `<body class="canvas">`) || !strings.Contains(page, `src="/dashboard/threads/`+home.ID+`/view"`) || !strings.Contains(page, `href="/dashboard/feed"`) {
		t.Error("the front page should be the chosen thread's view, with a way to the feed")
	}
	if _, _, page := e.do(reader, "GET", "/dashboard/feed", nil); !strings.Contains(page, "<h1>Activity</h1>") || !strings.Contains(page, "Mission control") {
		t.Error("the feed should stay reachable")
	}
	if _, _, page := e.do(reader, "GET", "/dashboard?q=words", nil); !strings.Contains(page, "<h1>Activity</h1>") {
		t.Error("a search should show results, not the home page")
	}
	// Deleting the thread, or choosing it again, gives the front page back.
	e.do(admin, "POST", "/admin/threads/"+home.ID+"/home", url.Values{})
	if _, _, page := e.do(reader, "GET", "/dashboard", nil); !strings.Contains(page, "<h1>Activity</h1>") {
		t.Error("unsetting home should bring the feed back")
	}
	e.do(admin, "POST", "/admin/threads/"+home.ID+"/home", url.Values{})
	e.call("DELETE", "/api/v1/threads/"+home.ID, key, nil, nil)
	if s, _, page := e.do(reader, "GET", "/dashboard", nil); s != 200 || !strings.Contains(page, "<h1>Activity</h1>") {
		t.Errorf("a deleted home thread should fall back to the feed: status %d", s)
	}
}
