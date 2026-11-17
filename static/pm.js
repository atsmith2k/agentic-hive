// hive/pm: project management for a thread. A board, a backlog with sprints,
// a roadmap of epics and releases, reports, and a detail panel per ticket,
// all drawn from the thread's data store. Agents run the project by writing
// that store over the API; the view follows within seconds. People the hive
// lets write (hive.viewer.can_write) can also drag, edit and comment.
//
// USE IT
//   "view": {"html": "<div data-pm></div>", "kits": ["hive/pm@1"]}
//   or call pm.mount(element, {tab: "board"}) yourself.
//
// THE DATA  (PUT /api/v1/threads/{id}/data/{name}; every name is optional)
//   project        {"name", "key": "PM", "description",
//                   "statuses": [{"id": "todo", "name": "To do", "category": "todo", "wip": 5}, ...],
//                   "types": [...], "priorities": [...], "members": [...]}
//                  category is todo, doing or done. Defaults: backlog, todo,
//                  in-progress, in-review, done; epic, story, task, bug;
//                  highest, high, medium, low, lowest.
//   ticket.<id>    {"title", "type", "status", "priority", "assignee", "reporter",
//                   "points", "labels": [], "sprint": "<sprint id>", "epic": "<ticket id>",
//                   "parent": "<ticket id>", "blocked_by": ["<ticket id>"], "release": "<release id>",
//                   "start", "due", "description", "flagged": true, "rank": 1,
//                   "links": [{"title", "url"}], "comments": [{"by", "at", "text"}],
//                   "created_at", "started_at", "done_at",
//                   "history": [{"at", "by", "field", "from", "to"}]}
//                  <id> is letters, digits or _: use the next number. The ticket
//                  is shown as KEY-<id>. An epic is a ticket of type "epic".
//                  Set started_at and done_at when you move a ticket: the
//                  burndown and velocity read them. Dates are YYYY-MM-DD or ISO.
//   sprint.<id>    {"name", "goal", "start", "end", "state": "planned" | "active" | "closed",
//                   "completed": {"done_points", "done_count", "total_points", "total_count"}}
//   release.<id>   {"name", "date", "released": true, "notes"}
//   archive.<n>    {"<ticket id>": {ticket}, ...}  finished tickets packed into
//                  one name, to stay under the store's limit of names. They
//                  stay in search, epics and reports, and leave the board.
//
// FROM A SCRIPT
//   pm.model()             {project, tickets, sprints, releases, byId}, normalised
//   pm.read(store)         the same, from any hive.data.all()
//   pm.onChange(fn)        fn(model) when the data changes
//   pm.open(id)            show a ticket
//   await pm.create(fields), pm.update(id, patch), pm.comment(id, text)
//                          save, for people the hive lets write
(function () {
  "use strict";
  var hive = window.hive;
  var DEF = {
    statuses: [
      { id: "backlog", name: "Backlog", category: "todo" },
      { id: "todo", name: "To do", category: "todo" },
      { id: "in-progress", name: "In progress", category: "doing" },
      { id: "in-review", name: "In review", category: "doing" },
      { id: "done", name: "Done", category: "done" },
    ],
    types: ["epic", "story", "task", "bug"],
    priorities: ["highest", "high", "medium", "low", "lowest"],
  };
  var PRIORITY_MARK = { highest: "⇈", high: "↑", medium: "=", low: "↓", lowest: "⇊" };
  var DAY = 86400000;

  // ---- small helpers ----
  function str(v) { return v == null ? "" : String(v).trim(); }
  function obj(v) { return v && typeof v === "object" && !Array.isArray(v) ? v : {}; }
  function arr(v) { return Array.isArray(v) ? v : []; }
  function strs(v) { return arr(v).map(str).filter(Boolean); }
  function num(v) { var n = typeof v === "string" && v.trim() === "" ? NaN : Number(v); return v == null || isNaN(n) ? null : n; }
  function slug(v) { return str(v).toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-|-$/g, ""); }
  function date(v) {
    if (!v) return null;
    if (v instanceof Date) return v;
    var s = String(v), d = new Date(/^\d{4}-\d{2}-\d{2}$/.test(s) ? s + "T00:00:00" : s);
    return isNaN(d) ? null : d;
  }
  function ymd(d) { return d.getFullYear() + "-" + ("0" + (d.getMonth() + 1)).slice(-2) + "-" + ("0" + d.getDate()).slice(-2); }
  function shortDate(v) { var d = date(v); return d ? d.toLocaleDateString(undefined, { month: "short", day: "numeric" }) : ""; }
  function ago(v) {
    var d = date(v);
    if (!d) return "";
    var s = (Date.now() - d) / 1000;
    if (s < 60) return "just now";
    if (s < 3600) return Math.floor(s / 60) + "m ago";
    if (s < 86400) return Math.floor(s / 3600) + "h ago";
    if (s < 86400 * 30) return Math.floor(s / 86400) + "d ago";
    return shortDate(v);
  }
  function hue(name) { var n = 0; for (var i = 0; i < name.length; i++) n = (n * 31 + name.charCodeAt(i)) % 360; return n; }
  function initials(name) {
    var parts = name.split(/[\s._@-]+/).filter(Boolean);
    return ((parts[0] || "?")[0] + (parts[1] ? parts[1][0] : "")).toUpperCase();
  }

  function add(el, kids) {
    kids.forEach(function (k) {
      if (k == null || k === false) return;
      if (Array.isArray(k)) return add(el, k);
      el.appendChild(k.nodeType ? k : document.createTextNode(String(k)));
    });
    return el;
  }
  function make(el, attrs, kids) {
    var value;
    Object.keys(attrs || {}).forEach(function (k) {
      var v = attrs[k];
      if (v == null || v === false) return;
      if (k.slice(0, 2) === "on") el.addEventListener(k.slice(2), v);
      else if (k === "value") value = v;
      else el.setAttribute(k, v === true ? "" : v);
    });
    add(el, kids);
    if (value !== undefined) el.value = value;
    return el;
  }
  function h(tag, attrs) { return make(document.createElement(tag), attrs, Array.prototype.slice.call(arguments, 2)); }
  function svg(tag, attrs) { return make(document.createElementNS("http://www.w3.org/2000/svg", tag), attrs, Array.prototype.slice.call(arguments, 2)); }

  // ---- the model ----
  function read(store) {
    store = store || {};
    var p = obj(store.project && store.project.value);
    var project = {
      name: str(p.name) || (hive && hive.thread && hive.thread.title) || "Project",
      key: str(p.key).replace(/[^A-Za-z0-9]/g, "").toUpperCase() || "PM",
      description: str(p.description),
      statuses: arr(p.statuses).map(function (s) {
        s = typeof s === "string" ? { name: s } : obj(s);
        var id = slug(s.id || s.name), cat = str(s.category).toLowerCase();
        return { id: id, name: str(s.name) || id, category: cat === "doing" || cat === "done" ? cat : "todo", wip: num(s.wip) };
      }).filter(function (s) { return s.id; }),
      types: strs(p.types).map(function (t) { return t.toLowerCase(); }),
      priorities: strs(p.priorities).map(function (t) { return t.toLowerCase(); }),
      members: strs(p.members),
    };
    if (!project.statuses.length) project.statuses = DEF.statuses.map(function (s) { return Object.assign({ wip: null }, s); });
    if (!project.types.length) project.types = DEF.types.slice();
    if (!project.priorities.length) project.priorities = DEF.priorities.slice();
    if (!project.statuses.some(function (s) { return s.category === "done"; })) project.statuses[project.statuses.length - 1].category = "done";

    var prefix = project.key.toLowerCase() + "-";
    function ref(v) { var s = str(v); return s.toLowerCase().indexOf(prefix) === 0 ? s.slice(prefix.length) : s; }
    function status(v) {
      var want = slug(v);
      if (!want) return project.statuses[0].id;
      for (var i = 0; i < project.statuses.length; i++) {
        var s = project.statuses[i];
        if (s.id === want || slug(s.name) === want) return s.id;
      }
      // A status the project does not list still gets a column.
      project.statuses.splice(project.statuses.length - 1, 0, { id: want, name: str(v), category: "todo", wip: null, adhoc: true });
      return want;
    }
    function ticket(id, raw, entry, src) {
      raw = obj(raw);
      var t = {
        id: id, key: project.key + "-" + id, src: src, archived: src.indexOf("archive.") === 0, raw: raw,
        title: str(raw.title) || "(untitled)", type: str(raw.type).toLowerCase() || "task", status: status(raw.status),
        priority: str(raw.priority).toLowerCase() || "medium", assignee: str(raw.assignee), reporter: str(raw.reporter),
        points: num(raw.points), labels: strs(raw.labels), sprint: str(raw.sprint), epic: ref(raw.epic), parent: ref(raw.parent),
        blocked_by: strs(raw.blocked_by).map(ref), release: str(raw.release), start: str(raw.start), due: str(raw.due),
        description: str(raw.description), flagged: raw.flagged === true, rank: num(raw.rank),
        links: arr(raw.links).map(obj), comments: arr(raw.comments).map(obj), history: arr(raw.history).map(obj),
        created_at: str(raw.created_at), started_at: str(raw.started_at), done_at: str(raw.done_at),
        updated_by: str(entry.updated_by), updated_at: str(entry.updated_at),
      };
      return t;
    }

    var byId = {};
    var names = Object.keys(store).sort();
    names.forEach(function (name) {
      if (name.indexOf("archive.") !== 0) return;
      var pack = obj(store[name].value);
      Object.keys(pack).forEach(function (id) { if (/^[A-Za-z0-9_]+$/.test(id)) byId[id] = ticket(id, pack[id], store[name], name); });
    });
    names.forEach(function (name) {
      var m = /^ticket\.([A-Za-z0-9_]+)$/.exec(name);
      if (m) byId[m[1]] = ticket(m[1], store[name].value, store[name], name);
    });
    var category = {};
    project.statuses.forEach(function (s) { category[s.id] = s.category; });
    var tickets = Object.keys(byId).map(function (id) { return byId[id]; });
    tickets.forEach(function (t) {
      t.category = category[t.status];
      t.done = t.category === "done";
      if (t.done && !t.done_at) t.done_at = t.updated_at;
      t.is_epic = t.type === "epic";
    });
    tickets.forEach(function (t) {
      t.blockers = t.blocked_by.map(function (id) { return byId[id]; }).filter(function (b) { return b && !b.done; });
      t.blocked = !t.done && (t.flagged || t.blockers.length > 0);
      t.overdue = !t.done && !!t.due && date(t.due) && date(t.due) < new Date(ymd(new Date()) + "T00:00:00");
    });
    tickets.sort(function (a, b) {
      if (a.rank !== b.rank) return (a.rank == null ? Infinity : a.rank) - (b.rank == null ? Infinity : b.rank);
      var pa = project.priorities.indexOf(a.priority), pb = project.priorities.indexOf(b.priority);
      if (pa !== pb) return (pa < 0 ? 99 : pa) - (pb < 0 ? 99 : pb);
      return (Number(a.id) || 0) - (Number(b.id) || 0) || (a.id < b.id ? -1 : 1);
    });

    function named(kind, build) {
      var out = [];
      names.forEach(function (name) {
        var m = new RegExp("^" + kind + "\\.([A-Za-z0-9_]+)$").exec(name);
        if (m) out.push(build(m[1], obj(store[name].value)));
      });
      return out;
    }
    var order = { active: 0, planned: 1, closed: 2 };
    var sprints = named("sprint", function (id, v) {
      var state = str(v.state).toLowerCase();
      return { id: id, name: str(v.name) || "Sprint " + id, goal: str(v.goal), start: str(v.start), end: str(v.end),
        state: order[state] === undefined ? "planned" : state, completed: v.completed ? obj(v.completed) : null, raw: v };
    }).sort(function (a, b) { return order[a.state] - order[b.state] || (a.start < b.start ? -1 : a.start > b.start ? 1 : 0); });
    var releases = named("release", function (id, v) {
      return { id: id, name: str(v.name) || id, date: str(v.date), released: v.released === true, notes: str(v.notes), raw: v };
    }).sort(function (a, b) { return a.date < b.date ? -1 : a.date > b.date ? 1 : 0; });

    return { project: project, tickets: tickets, byId: byId, sprints: sprints, releases: releases, names: names.length };
  }

  // ---- state ----
  var root = null, model = read(hive ? hive.data.all() : {}), stale = false, toastTimer = 0;
  var ui = { tab: "board", q: "", assignee: "", type: "", label: "", scope: null, lanes: "", open: null, draft: null, sprintForm: null, releaseForm: false, closed: false };
  var watchers = [];
  function canWrite() { return !!(hive && hive.viewer && hive.viewer.can_write); }
  function weight(list) {
    // Points when the tickets carry them, otherwise a count.
    var pointed = list.some(function (t) { return t.points != null; });
    return { unit: pointed ? "points" : "tickets", of: function (t) { return pointed ? t.points || 0 : 1; } };
  }
  function total(list, w) { return list.reduce(function (n, t) { return n + w.of(t); }, 0); }
  function sprintTickets(id) { return model.tickets.filter(function (t) { return t.sprint === id && !t.is_epic; }); }
  function activeSprint() { return model.sprints.filter(function (s) { return s.state === "active"; })[0] || null; }
  function matches(t) {
    if (ui.assignee && (ui.assignee === "-" ? t.assignee : t.assignee !== ui.assignee)) return false;
    if (ui.type && t.type !== ui.type) return false;
    if (ui.label && t.labels.indexOf(ui.label) < 0) return false;
    if (!ui.q) return true;
    var q = ui.q.toLowerCase();
    return [t.key, t.title, t.description, t.assignee, t.labels.join(" ")].join("\n").toLowerCase().indexOf(q) >= 0;
  }

  // ---- saving ----
  function toast(text) {
    if (!root) return;
    var el = root.querySelector(".pm-toast");
    el.textContent = text;
    el.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(function () { el.hidden = true; }, 5000);
  }
  function guard(promise) {
    return promise.catch(function (err) { toast("Not saved: " + (err && err.message ? err.message : err)); throw err; });
  }
  function put(name, value) { return guard(hive.data.set(name, value)); }
  var TRACKED = ["status", "assignee", "priority", "sprint", "points", "type", "epic", "release", "due", "title"];
  function changed(raw, patch, by) {
    var next = Object.assign({}, raw), now = new Date().toISOString();
    var log = arr(raw.history).slice();
    Object.keys(patch).forEach(function (k) {
      var from = raw[k], to = patch[k];
      if (to == null || to === "" || (Array.isArray(to) && !to.length)) { delete next[k]; to = null; } else next[k] = to;
      if (TRACKED.indexOf(k) >= 0 && str(from) !== str(to)) log.push({ at: now, by: by, field: k, from: from == null ? null : from, to: to });
    });
    if ("status" in patch && slug(patch.status) !== slug(raw.status)) {
      var cat = (model.project.statuses.filter(function (s) { return s.id === slug(patch.status); })[0] || {}).category;
      if (cat === "done") next.done_at = now; else delete next.done_at;
      if (cat !== "todo" && !next.started_at) next.started_at = now;
    }
    if (log.length) next.history = log.slice(-40);
    return next;
  }
  function update(id, patch) {
    var t = read(hive.data.all()).byId[id];
    if (!t) return Promise.reject(new Error("there is no ticket " + id));
    var next = changed(t.raw, patch, "person");
    if (!t.archived) return put("ticket." + id, next);
    var pack = Object.assign({}, obj(hive.data.get(t.src)));
    pack[id] = next;
    return put(t.src, pack);
  }
  function nextId(list) {
    return String(list.reduce(function (n, x) { return Math.max(n, Number(x.id) || 0); }, 0) + 1);
  }
  function create(fields) {
    var id = nextId(read(hive.data.all()).tickets);
    var raw = changed({ created_at: new Date().toISOString(), reporter: "person" }, fields || {}, "person");
    delete raw.history;
    return put("ticket." + id, raw).then(function () { return id; });
  }
  function comment(id, text) {
    var t = read(hive.data.all()).byId[id];
    if (!t || !str(text)) return Promise.resolve();
    return update(id, { comments: t.comments.concat([{ by: "person", at: new Date().toISOString(), text: str(text) }]) });
  }
  function removeTicket(t) {
    if (!t.archived) return guard(hive.data.remove("ticket." + t.id));
    var pack = Object.assign({}, obj(hive.data.get(t.src)));
    delete pack[t.id];
    return put(t.src, pack);
  }
  function chain(steps) { return steps.reduce(function (p, step) { return p.then(step); }, Promise.resolve()); }
  function completeSprint(s) {
    var list = sprintTickets(s.id), w = weight(list);
    var done = list.filter(function (t) { return t.done; });
    var snapshot = { done_points: total(done, { of: function (t) { return t.points || 0; } }), done_count: done.length,
      total_points: total(list, { of: function (t) { return t.points || 0; } }), total_count: list.length, unit: w.unit };
    var steps = list.filter(function (t) { return !t.done; }).map(function (t) { return function () { return update(t.id, { sprint: null }); }; });
    steps.push(function () { return put("sprint." + s.id, Object.assign({}, s.raw, { state: "closed", completed: snapshot, end: s.end || ymd(new Date()) })); });
    return chain(steps);
  }
  // archiveDone packs finished tickets of closed sprints into archive.<n>
  // values, so the store's names are free for new work.
  function archiveDone() {
    var closed = {};
    model.sprints.forEach(function (s) { if (s.state === "closed") closed[s.id] = true; });
    var list = model.tickets.filter(function (t) { return t.done && !t.archived && !t.is_epic && (closed[t.sprint] || (date(t.done_at) && Date.now() - date(t.done_at) > 30 * DAY)); });
    if (!list.length) return toast("Nothing to archive: only finished tickets of closed sprints, or done for 30 days, are packed.");
    var store = hive.data.all(), n = 1, packs = [];
    function fresh() { while (store["archive." + n]) n++; var p = { name: "archive." + n++, value: {}, size: 2 }; packs.push(p); return p; }
    var pack = fresh();
    list.forEach(function (t) {
      var size = JSON.stringify(t.raw).length + t.id.length + 4;
      if (pack.size + size > 48000) pack = fresh();
      pack.value[t.id] = t.raw;
      pack.size += size;
    });
    var steps = packs.map(function (p) { return function () { return put(p.name, p.value); }; });
    list.forEach(function (t) { steps.push(function () { return guard(hive.data.remove("ticket." + t.id)); }); });
    return chain(steps).then(function () { toast("Archived " + list.length + " finished tickets."); });
  }

  // ---- drawing: pieces ----
  function avatar(name) {
    if (!name) return h("span", { class: "pm-avatar pm-none", title: "Unassigned" }, "–");
    var el = h("span", { class: "pm-avatar", title: name }, initials(name));
    el.style.background = "hsl(" + hue(name) + " 45% 38%)";
    return el;
  }
  function typeMark(type) { return h("span", { class: "pm-type pm-type-" + slug(type), title: type }, (type[0] || "?").toUpperCase()); }
  function priorityMark(p) { return h("span", { class: "pm-prio pm-prio-" + slug(p), title: "Priority: " + p }, PRIORITY_MARK[p] || "•"); }
  function statusPill(t) {
    var s = model.project.statuses.filter(function (x) { return x.id === t.status; })[0];
    return h("span", { class: "pm-status pm-cat-" + t.category }, s ? s.name : t.status);
  }
  function keyLink(t) {
    return h("a", { class: "pm-key" + (t.done ? " pm-struck" : ""), href: "#", onclick: function (e) { e.preventDefault(); open(t.id); } }, t.key);
  }
  function bar(done, all, label) {
    var pct = all ? Math.round((done / all) * 100) : 0;
    var fill = h("span", { class: "pm-bar-fill" });
    fill.style.width = pct + "%";
    return h("span", { class: "pm-bar", title: label || done + " of " + all }, fill, h("span", { class: "pm-bar-text" }, pct + "%"));
  }
  // rich turns text into nodes: links and ticket keys become clickable.
  function rich(text) {
    var re = new RegExp("(https?://[^\\s<>\"')\\]]+|\\b" + model.project.key + "-[A-Za-z0-9_]+\\b)", "g");
    return text.split(re).map(function (part, i) {
      if (i % 2 === 0) return part;
      if (/^https?:/.test(part)) return h("a", { href: part, target: "_blank", rel: "noopener noreferrer" }, part);
      var t = model.byId[part.slice(model.project.key.length + 1)];
      return t ? keyLink(t) : part;
    });
  }
  function draggable(el, kind, id) {
    if (!canWrite()) return el;
    el.setAttribute("draggable", "true");
    el.addEventListener("dragstart", function (e) {
      e.dataTransfer.setData("text/plain", kind + ":" + id);
      e.dataTransfer.effectAllowed = "move";
      el.classList.add("pm-dragging");
    });
    el.addEventListener("dragend", function () { el.classList.remove("pm-dragging"); });
    return el;
  }
  function droppable(el, ondrop) {
    if (!canWrite()) return el;
    el.addEventListener("dragover", function (e) { e.preventDefault(); e.dataTransfer.dropEffect = "move"; el.classList.add("pm-over"); });
    el.addEventListener("dragleave", function () { el.classList.remove("pm-over"); });
    el.addEventListener("drop", function (e) {
      e.preventDefault();
      el.classList.remove("pm-over");
      var m = /^ticket:(.+)$/.exec(e.dataTransfer.getData("text/plain"));
      if (m && model.byId[m[1]]) ondrop(model.byId[m[1]]);
    });
    return el;
  }
  function select(focus, value, options, onpick, label) {
    var el = h("select", { "data-f": focus, "aria-label": label, onchange: function () { onpick(el.value); } },
      options.map(function (o) { return h("option", { value: o[0] }, o[1]); }));
    if (!options.some(function (o) { return o[0] === value; })) el.appendChild(h("option", { value: value }, value));
    el.value = value;
    return el;
  }
  function empty(text) { return h("p", { class: "pm-empty" }, text); }

  // ---- drawing: board ----
  function card(t) {
    var epic = t.epic && model.byId[t.epic];
    var kids = model.tickets.filter(function (k) { return k.parent === t.id; });
    var el = h("article", { class: "pm-card" + (t.blocked ? " pm-blocked" : ""), tabindex: "0", "data-ticket": t.id,
        onclick: function () { open(t.id); }, onkeydown: function (e) { if (e.key === "Enter") open(t.id); } },
      h("div", { class: "pm-card-title" }, t.title),
      epic ? h("span", { class: "pm-chip pm-epic", title: "Epic " + epic.key }, epic.title) : null,
      t.labels.map(function (l) { return h("span", { class: "pm-chip" }, l); }),
      h("div", { class: "pm-card-foot" },
        typeMark(t.type), h("span", { class: "pm-key" + (t.done ? " pm-struck" : "") }, t.key), priorityMark(t.priority),
        t.blocked ? h("span", { class: "pm-flag", title: t.blockers.length ? "Blocked by " + t.blockers.map(function (b) { return b.key; }).join(", ") : "Flagged" }, "blocked") : null,
        t.overdue ? h("span", { class: "pm-flag", title: "Due " + t.due }, "overdue") : (t.due && !t.done ? h("span", { class: "pm-due" }, shortDate(t.due)) : null),
        kids.length ? h("span", { class: "pm-due", title: "Sub-tasks done" }, kids.filter(function (k) { return k.done; }).length + "/" + kids.length) : null,
        h("span", { class: "pm-spacer" }),
        t.points != null ? h("span", { class: "pm-points", title: "Points" }, t.points) : null,
        avatar(t.assignee)));
    return draggable(el, "ticket", t.id);
  }
  function boardScope() {
    if (ui.scope != null && (ui.scope === "all" || ui.scope === "none" || model.sprints.some(function (s) { return s.id === ui.scope; }))) return ui.scope;
    var a = activeSprint();
    return a ? a.id : "all";
  }
  function drawBoard() {
    var scope = boardScope(), hidden = 0;
    var list = model.tickets.filter(function (t) {
      if (t.is_epic || t.archived || !matches(t)) return false;
      if (scope === "none") return !t.sprint;
      if (scope !== "all") return t.sprint === scope;
      if (t.done && date(t.done_at) && Date.now() - date(t.done_at) > 14 * DAY) { hidden++; return false; }
      return true;
    });
    var options = model.sprints.filter(function (s) { return s.state !== "closed" || s.id === scope; })
      .map(function (s) { return [s.id, s.name + (s.state === "active" ? " (active)" : s.state === "closed" ? " (closed)" : "")]; })
      .concat([["all", "All tickets"], ["none", "No sprint"]]);
    var sprint = model.sprints.filter(function (s) { return s.id === scope; })[0];
    var tools = h("div", { class: "pm-tools" },
      select("scope", scope, options, function (v) { ui.scope = v; render(); }, "Sprint"),
      select("lanes", ui.lanes, [["", "No swimlanes"], ["assignee", "By assignee"], ["epic", "By epic"]], function (v) { ui.lanes = v; render(); }, "Swimlanes"),
      sprint ? h("span", { class: "pm-muted" }, [sprint.start && sprint.end ? shortDate(sprint.start) + " – " + shortDate(sprint.end) : "", sprint.goal].filter(Boolean).join(" · ")) : null);

    var lanes = [{ title: "", list: list }];
    if (ui.lanes) {
      var groups = {}, order = [];
      list.forEach(function (t) {
        var k = ui.lanes === "assignee" ? t.assignee : t.epic;
        if (!groups[k]) { groups[k] = []; order.push(k); }
        groups[k].push(t);
      });
      order.sort(function (a, b) { return a === "" ? 1 : b === "" ? -1 : a < b ? -1 : 1; });
      lanes = order.map(function (k) {
        var epic = ui.lanes === "epic" && model.byId[k];
        return { title: k === "" ? (ui.lanes === "assignee" ? "Unassigned" : "No epic") : epic ? epic.key + " " + epic.title : k, list: groups[k] };
      });
    }
    var statuses = model.project.statuses;
    var counts = {};
    list.forEach(function (t) { counts[t.status] = (counts[t.status] || 0) + 1; });
    var grid = h("div", { class: "pm-board" });
    grid.style.gridTemplateColumns = "repeat(" + statuses.length + ", minmax(220px, 1fr))";
    statuses.forEach(function (s) {
      var n = counts[s.id] || 0;
      grid.appendChild(h("div", { class: "pm-col-head pm-cat-" + s.category + (s.wip != null && n > s.wip ? " pm-over-wip" : "") },
        h("span", { class: "pm-dot" }), s.name, h("span", { class: "pm-count" }, s.wip != null ? n + " / " + s.wip : n)));
    });
    lanes.forEach(function (lane) {
      if (lane.title) {
        var head = h("div", { class: "pm-lane" }, lane.title, h("span", { class: "pm-count" }, lane.list.length));
        head.style.gridColumn = "1 / -1";
        grid.appendChild(head);
      }
      statuses.forEach(function (s) {
        var cell = h("div", { class: "pm-col", "data-status": s.id },
          lane.list.filter(function (t) { return t.status === s.id; }).map(card));
        grid.appendChild(droppable(cell, function (t) { if (t.status !== s.id) update(t.id, { status: s.id }); }));
      });
    });
    return [tools, list.length || model.tickets.length ? h("div", { class: "pm-scroll" }, grid) : empty("No tickets yet. An agent adds one by writing ticket.1 to this thread's data."),
      hidden ? h("p", { class: "pm-muted" }, hidden + " tickets finished more than two weeks ago are not shown here. Find them in the backlog.") : null];
  }

  // ---- drawing: backlog ----
  function row(t) {
    var epic = t.epic && model.byId[t.epic];
    return draggable(h("tr", { class: "pm-row" + (t.blocked ? " pm-blocked" : ""), "data-ticket": t.id, onclick: function () { open(t.id); } },
      h("td", { class: "pm-nowrap" }, typeMark(t.type), " ", keyLink(t)),
      h("td", { class: "pm-grow" }, t.title, epic ? h("span", { class: "pm-chip pm-epic" }, epic.title) : null,
        t.labels.map(function (l) { return h("span", { class: "pm-chip" }, l); }),
        t.blocked ? h("span", { class: "pm-flag" }, "blocked") : null, t.overdue ? h("span", { class: "pm-flag" }, "overdue") : null),
      h("td", null, statusPill(t)),
      h("td", null, priorityMark(t.priority)),
      h("td", { class: "pm-num" }, t.points != null ? h("span", { class: "pm-points" }, t.points) : ""),
      h("td", null, avatar(t.assignee))), "ticket", t.id);
  }
  function tableOf(list) {
    return h("table", { class: "pm-table" }, h("tbody", null, list.map(row)));
  }
  function sprintForm(s) {
    var f = ui.sprintForm;
    function input(name, type, label) {
      return h("label", { class: "pm-field" }, h("span", { class: "pm-label" }, label),
        h("input", { type: type, value: f[name] || "", "data-f": "sprint-" + name, oninput: function (e) { f[name] = e.target.value; } }));
    }
    return h("div", { class: "pm-form pm-inline" }, input("name", "text", "Name"), input("goal", "text", "Goal"), input("start", "date", "Start"), input("end", "date", "End"),
      h("button", { onclick: function () {
        var id = s ? s.id : nextId(model.sprints);
        var value = Object.assign({}, s ? s.raw : { state: "planned" }, { name: str(f.name) || "Sprint " + id, goal: str(f.goal), start: f.start || "", end: f.end || "" });
        ui.sprintForm = null;
        put("sprint." + id, value).catch(function () {});
        render();
      } }, "Save sprint"),
      h("button", { class: "pm-quiet", onclick: function () { ui.sprintForm = null; render(); } }, "Cancel"));
  }
  function section(title, meta, list, actions, ondrop, body) {
    var w = weight(list);
    var el = h("section", { class: "pm-section" },
      h("header", { class: "pm-section-head" }, h("h3", null, title), h("span", { class: "pm-muted" }, meta),
        h("span", { class: "pm-spacer" }),
        h("span", { class: "pm-muted" }, list.length + " tickets" + (w.unit === "points" ? " · " + total(list, w) + " points" : "")), actions),
      body, list.length ? tableOf(list) : h("p", { class: "pm-empty pm-drop-hint" }, canWrite() ? "Nothing here. Drag a ticket in." : "Nothing here."));
    return ondrop ? droppable(el, ondrop) : el;
  }
  function drawBacklog() {
    var out = [], write = canWrite();
    var visible = model.tickets.filter(function (t) { return !t.is_epic && matches(t); });
    model.sprints.filter(function (s) { return s.state !== "closed"; }).forEach(function (s) {
      var list = visible.filter(function (t) { return t.sprint === s.id; });
      var editing = ui.sprintForm && ui.sprintForm.id === s.id;
      var actions = write ? [
        s.state === "planned" ? h("button", { onclick: function () { put("sprint." + s.id, Object.assign({}, s.raw, { state: "active", start: s.start || ymd(new Date()) })).catch(function () {}); } }, "Start sprint")
          : confirmButton("Complete sprint", "Complete: unfinished go to backlog", function () { completeSprint(s).catch(function () {}); }),
        h("button", { class: "pm-quiet", onclick: function () { ui.sprintForm = { id: s.id, name: s.name, goal: s.goal, start: s.start, end: s.end }; render(); } }, "Edit"),
      ] : null;
      out.push(section(s.name, [s.state === "active" ? "active" : "planned", s.start && s.end ? shortDate(s.start) + " – " + shortDate(s.end) : "", s.goal].filter(Boolean).join(" · "),
        list, actions, function (t) { if (t.sprint !== s.id) update(t.id, { sprint: s.id }); }, editing ? sprintForm(s) : null));
    });
    var backlog = visible.filter(function (t) { return !t.done && (!t.sprint || !model.sprints.some(function (s) { return s.id === t.sprint; })); });
    out.push(section("Backlog", "not in a sprint", backlog,
      write ? h("button", { class: "pm-quiet", onclick: function () { ui.sprintForm = { id: null }; render(); } }, "New sprint") : null,
      function (t) { if (t.sprint) update(t.id, { sprint: null }); }, ui.sprintForm && ui.sprintForm.id === null ? sprintForm(null) : null));
    var finished = visible.filter(function (t) { return backlog.indexOf(t) < 0 && (t.done ? !model.sprints.some(function (s) { return s.id === t.sprint && s.state !== "closed"; }) : model.sprints.some(function (s) { return s.id === t.sprint && s.state === "closed"; })); });
    if (finished.length) {
      var more = h("details", { class: "pm-section", ontoggle: function () { ui.closed = more.open; } },
        h("summary", null, "Finished and closed sprints", h("span", { class: "pm-count" }, finished.length)),
        write ? h("p", null, h("button", { class: "pm-quiet", onclick: function () { var p = archiveDone(); if (p) p.catch(function () {}); } }, "Archive finished tickets"),
          h("span", { class: "pm-muted" }, " packs them into one name of the data store; they stay searchable.")) : null,
        tableOf(finished));
      more.open = ui.closed || !!ui.q;
      out.push(more);
    }
    return out;
  }
  // The frame's sandbox has no confirm(): a second click on the same button confirms.
  function confirmButton(label, sure, fn) {
    var armed = false;
    var b = h("button", { onclick: function (e) {
      e.stopPropagation();
      if (armed) return fn();
      armed = true;
      b.textContent = sure;
      b.classList.add("pm-danger");
      setTimeout(function () { armed = false; b.textContent = label; b.classList.remove("pm-danger"); }, 4000);
    } }, label);
    return b;
  }

  // ---- drawing: roadmap ----
  function drawRoadmap() {
    var out = [];
    var epics = model.tickets.filter(function (t) { return t.is_epic && matches(t); });
    var dated = epics.filter(function (e) { return date(e.start) && date(e.due) && date(e.due) >= date(e.start); });
    if (dated.length) {
      var from = Math.min.apply(null, dated.map(function (e) { return +date(e.start); })), to = Math.max.apply(null, dated.map(function (e) { return +date(e.due); })) + DAY;
      var pos = function (ms) { return Math.max(0, Math.min(100, ((ms - from) / (to - from)) * 100)); };
      var chart = h("div", { class: "pm-gantt" });
      dated.forEach(function (e) {
        var b = h("button", { class: "pm-gantt-bar pm-cat-" + e.category, title: e.start + " to " + e.due, onclick: function () { open(e.id); } }, e.key + " " + e.title);
        b.style.marginLeft = pos(+date(e.start)) + "%";
        b.style.width = Math.max(4, pos(+date(e.due) + DAY) - pos(+date(e.start))) + "%";
        chart.appendChild(h("div", { class: "pm-gantt-row" }, b));
      });
      model.releases.concat([{ name: "Today", date: ymd(new Date()), today: true }]).forEach(function (r) {
        var d = date(r.date);
        if (!d || +d < from || +d > to) return;
        var mark = h("span", { class: "pm-gantt-mark" + (r.today ? " pm-today" : ""), title: r.name + " " + r.date }, h("span", null, r.name));
        mark.style.left = pos(+d) + "%";
        chart.appendChild(mark);
      });
      out.push(h("section", { class: "pm-section" }, h("header", { class: "pm-section-head" }, h("h3", null, "Timeline"),
        h("span", { class: "pm-muted" }, shortDate(new Date(from)) + " – " + shortDate(new Date(to - DAY)))), chart));
    }
    out.push(h("section", { class: "pm-section" }, h("header", { class: "pm-section-head" }, h("h3", null, "Epics")),
      epics.length ? h("div", { class: "pm-epics" }, epics.map(function (e) {
        var kids = model.tickets.filter(function (t) { return t.epic === e.id; }), w = weight(kids);
        return h("article", { class: "pm-epic-card", tabindex: "0", onclick: function () { open(e.id); }, onkeydown: function (ev) { if (ev.key === "Enter") open(e.id); } },
          h("div", { class: "pm-card-foot" }, typeMark("epic"), h("span", { class: "pm-key" }, e.key), statusPill(e), h("span", { class: "pm-spacer" }), e.due ? h("span", { class: e.overdue ? "pm-flag" : "pm-due" }, "due " + shortDate(e.due)) : null, avatar(e.assignee)),
          h("div", { class: "pm-card-title" }, e.title),
          bar(total(kids.filter(function (t) { return t.done; }), w), total(kids, w)),
          h("div", { class: "pm-muted" }, kids.filter(function (t) { return t.done; }).length + " of " + kids.length + " tickets done" + (kids.some(function (t) { return t.blocked; }) ? " · " + kids.filter(function (t) { return t.blocked; }).length + " blocked" : "")));
      })) : empty("No epics. A ticket of type \"epic\" is one; other tickets point at it with \"epic\".")));

    var write = canWrite();
    var form = ui.releaseForm ? (function () {
      var f = ui.releaseForm;
      return h("div", { class: "pm-form pm-inline" },
        h("label", { class: "pm-field" }, h("span", { class: "pm-label" }, "Name"), h("input", { type: "text", value: f.name || "", "data-f": "release-name", oninput: function (e) { f.name = e.target.value; } })),
        h("label", { class: "pm-field" }, h("span", { class: "pm-label" }, "Date"), h("input", { type: "date", value: f.date || "", "data-f": "release-date", oninput: function (e) { f.date = e.target.value; } })),
        h("button", { onclick: function () {
          if (!str(f.name)) return toast("A release needs a name.");
          var id = slug(f.name).replace(/-/g, "_") || nextId(model.releases);
          ui.releaseForm = false;
          put("release." + id, { name: str(f.name), date: f.date || "" }).catch(function () {});
          render();
        } }, "Save release"),
        h("button", { class: "pm-quiet", onclick: function () { ui.releaseForm = false; render(); } }, "Cancel"));
    })() : null;
    out.push(h("section", { class: "pm-section" },
      h("header", { class: "pm-section-head" }, h("h3", null, "Releases"), h("span", { class: "pm-spacer" }),
        write ? h("button", { class: "pm-quiet", onclick: function () { ui.releaseForm = {}; render(); } }, "New release") : null),
      form,
      model.releases.length ? h("table", { class: "pm-table" }, h("tbody", null, model.releases.map(function (r) {
        var list = model.tickets.filter(function (t) { return t.release === r.id && !t.is_epic; }), w = weight(list);
        return h("tr", null,
          h("td", { class: "pm-nowrap" }, h("strong", null, r.name)),
          h("td", null, h("span", { class: "pm-status pm-cat-" + (r.released ? "done" : "doing") }, r.released ? "released" : "planned")),
          h("td", { class: "pm-nowrap pm-muted" }, r.date ? shortDate(r.date) : "no date"),
          h("td", { class: "pm-grow" }, bar(total(list.filter(function (t) { return t.done; }), w), total(list, w)), h("span", { class: "pm-muted" }, " " + list.filter(function (t) { return t.done; }).length + " of " + list.length + " tickets"),
            r.notes ? h("div", { class: "pm-muted" }, r.notes) : null),
          h("td", null, write ? h("button", { class: "pm-quiet", onclick: function () { put("release." + r.id, Object.assign({}, r.raw, { released: !r.released })).catch(function () {}); } }, r.released ? "Reopen" : "Mark released") : null));
      }))) : empty("No releases. A ticket joins one with \"release\".")));
    return out;
  }

  // ---- drawing: reports ----
  function tile(label, value, tone) { return h("div", { class: "pm-tile" + (tone ? " pm-tone-" + tone : "") }, h("div", { class: "pm-tile-value" }, value), h("div", { class: "pm-label" }, label)); }
  function burndown(s) {
    var list = sprintTickets(s.id), w = weight(list), start = date(s.start), end = date(s.end);
    if (!start || !end || end < start || !list.length) return empty("The burndown needs a sprint with start and end dates and tickets in it.");
    var days = Math.round((end - start) / DAY) + 1, all = total(list, w), today = new Date();
    var W = 560, H = 200, L = 34, B = 22, T = 8;
    var x = function (i) { return L + (i / Math.max(1, days)) * (W - L - 6); }, y = function (v) { return T + (1 - v / Math.max(1, all)) * (H - T - B); };
    var actual = [[x(0), y(all)]];
    for (var i = 0; i < days; i++) {
      var close = new Date(+start + (i + 1) * DAY);
      if (+start + i * DAY > +today) break;
      var left = all - total(list.filter(function (t) { return t.done && date(t.done_at) && date(t.done_at) < close; }), w);
      actual.push([x(i + 1), y(left)]);
    }
    var pts = function (a) { return a.map(function (p) { return p[0].toFixed(1) + "," + p[1].toFixed(1); }).join(" "); };
    return h("figure", { class: "pm-chart" },
      svg("svg", { viewBox: "0 0 " + W + " " + H, role: "img", "aria-label": "Burndown of " + s.name },
        svg("line", { class: "pm-axis", x1: L, y1: T, x2: L, y2: H - B }), svg("line", { class: "pm-axis", x1: L, y1: H - B, x2: W - 6, y2: H - B }),
        svg("text", { class: "pm-tick", x: L - 6, y: T + 8, "text-anchor": "end" }, all), svg("text", { class: "pm-tick", x: L - 6, y: H - B, "text-anchor": "end" }, "0"),
        svg("text", { class: "pm-tick", x: L, y: H - 6 }, shortDate(s.start)), svg("text", { class: "pm-tick", x: W - 6, y: H - 6, "text-anchor": "end" }, shortDate(s.end)),
        svg("line", { class: "pm-ideal", x1: x(0), y1: y(all), x2: x(days), y2: y(0) }),
        svg("polyline", { class: "pm-actual", points: pts(actual) })),
      h("figcaption", { class: "pm-muted" }, s.name + ": " + w.unit + " left, against a straight line to zero."));
  }
  function drawReports() {
    var all = model.tickets.filter(function (t) { return !t.is_epic; }), w = weight(all);
    var count = function (fn) { return all.filter(fn).length; };
    var out = [h("div", { class: "pm-tiles" },
      tile("Open", count(function (t) { return !t.done; })),
      tile("In progress", count(function (t) { return t.category === "doing"; }), "blue"),
      tile("Done", count(function (t) { return t.done; }), "green"),
      tile("Blocked", count(function (t) { return t.blocked; }), count(function (t) { return t.blocked; }) ? "red" : ""),
      tile("Overdue", count(function (t) { return t.overdue; }), count(function (t) { return t.overdue; }) ? "red" : ""),
      tile("Unassigned", count(function (t) { return !t.done && !t.assignee; })))];

    var stack = h("div", { class: "pm-stack" });
    model.project.statuses.forEach(function (s) {
      var n = total(all.filter(function (t) { return t.status === s.id; }), w);
      if (!n) return;
      var seg = h("span", { class: "pm-seg pm-cat-" + s.category, title: s.name + ": " + n + " " + w.unit }, s.name + " " + n);
      seg.style.flexGrow = n;
      stack.appendChild(seg);
    });
    out.push(h("section", { class: "pm-section" }, h("header", { class: "pm-section-head" }, h("h3", null, "Where the work is"), h("span", { class: "pm-muted" }, "by " + w.unit)),
      all.length ? stack : empty("No tickets yet.")));

    var sprint = activeSprint() || model.sprints.filter(function (s) { return s.state === "closed"; }).slice(-1)[0] || model.sprints[0];
    if (sprint) out.push(h("section", { class: "pm-section" }, h("header", { class: "pm-section-head" }, h("h3", null, "Burndown")), burndown(sprint)));

    var closed = model.sprints.filter(function (s) { return s.state === "closed"; });
    if (closed.length) {
      var vel = closed.map(function (s) {
        var list = sprintTickets(s.id).filter(function (t) { return t.done; }), c = s.completed;
        var points = w.unit === "points";
        return { s: s, v: c ? num(points ? c.done_points : c.done_count) || 0 : total(list, w) };
      });
      var max = Math.max.apply(null, vel.map(function (v) { return v.v; }).concat([1]));
      var avg = vel.reduce(function (n, v) { return n + v.v; }, 0) / vel.length;
      out.push(h("section", { class: "pm-section" }, h("header", { class: "pm-section-head" }, h("h3", null, "Velocity"), h("span", { class: "pm-muted" }, "average " + Math.round(avg * 10) / 10 + " " + w.unit + " a sprint")),
        h("div", { class: "pm-bars" }, vel.map(function (v) {
          var b = h("span", { class: "pm-vbar" });
          b.style.height = Math.max(2, (v.v / max) * 100) + "%";
          return h("div", { class: "pm-vcol", title: v.s.name + ": " + v.v + " " + w.unit }, h("span", { class: "pm-tick" }, v.v), b, h("span", { class: "pm-tick" }, v.s.name));
        }))));
    }

    var people = {};
    all.filter(function (t) { return !t.done; }).forEach(function (t) {
      var p = people[t.assignee] || (people[t.assignee] = { open: 0, doing: 0, load: 0, blocked: 0 });
      p.open++; p.load += w.of(t);
      if (t.category === "doing") p.doing++;
      if (t.blocked) p.blocked++;
    });
    var names = Object.keys(people).sort(function (a, b) { return people[b].load - people[a].load; });
    if (names.length) out.push(h("section", { class: "pm-section" }, h("header", { class: "pm-section-head" }, h("h3", null, "Workload"), h("span", { class: "pm-muted" }, "open tickets")),
      h("table", { class: "pm-table" }, h("thead", null, h("tr", null, h("th", null, "Assignee"), h("th", { class: "pm-num" }, "Open"), h("th", { class: "pm-num" }, "In progress"), h("th", { class: "pm-num" }, "Blocked"), h("th", { class: "pm-num" }, w.unit === "points" ? "Points" : "Tickets"))),
        h("tbody", null, names.map(function (n) {
          var p = people[n];
          return h("tr", { class: "pm-row", onclick: function () { ui.assignee = n || "-"; ui.tab = "backlog"; render(); } }, h("td", null, avatar(n), " ", n || "Unassigned"), h("td", { class: "pm-num" }, p.open), h("td", { class: "pm-num" }, p.doing), h("td", { class: "pm-num" }, p.blocked || ""), h("td", { class: "pm-num" }, p.load));
        })))));

    var recent = model.tickets.filter(function (t) { return t.updated_at; }).sort(function (a, b) { return a.updated_at < b.updated_at ? 1 : -1; }).slice(0, 12);
    if (recent.length) out.push(h("section", { class: "pm-section" }, h("header", { class: "pm-section-head" }, h("h3", null, "Recently changed")),
      h("table", { class: "pm-table" }, h("tbody", null, recent.map(function (t) {
        var last = t.history[t.history.length - 1];
        return h("tr", { class: "pm-row", onclick: function () { open(t.id); } }, h("td", { class: "pm-nowrap" }, typeMark(t.type), " ", keyLink(t)), h("td", { class: "pm-grow" }, t.title,
          last && last.field ? h("span", { class: "pm-muted" }, " · " + str(last.field) + " → " + (last.to == null ? "none" : str(last.to))) : null), h("td", null, statusPill(t)),
          h("td", { class: "pm-nowrap pm-muted" }, t.updated_by + " · " + ago(t.updated_at)));
      })))));
    return out;
  }

  // ---- drawing: one ticket ----
  function open(id) { ui.open = id; ui.draft = null; render(); var p = root && root.querySelector(".pm-detail"); if (p) p.focus(); }
  function close() { ui.open = null; ui.draft = null; render(); }
  function drawDetail() {
    var creating = !!ui.draft, t = creating ? null : model.byId[ui.open];
    if (!creating && !t) return null;
    var write = canWrite(), d = ui.draft, P = model.project;
    function val(name) { return creating ? d[name] : t[name]; }
    function save(name, value) {
      if (creating) { d[name] = value; return; }
      var patch = {}; patch[name] = value;
      update(t.id, patch).catch(function () {});
    }
    function field(label, control, wide) { return h("label", { class: "pm-field" + (wide ? " pm-wide" : "") }, h("span", { class: "pm-label" }, label), control); }
    function text(name, label, type, wide) {
      var v = val(name);
      v = Array.isArray(v) ? v.join(", ") : v == null ? "" : String(v);
      if (!write) return v ? field(label, h("span", null, name === "due" || name === "start" ? shortDate(v) || v : v), wide) : null;
      var list = name === "labels" || name === "blocked_by";
      return field(label, h("input", { type: type || "text", value: v, "data-f": name, list: name === "assignee" ? "pm-people" : null,
        oninput: creating ? function (e) { d[name] = list ? e.target.value.split(",").map(str).filter(Boolean) : type === "number" ? num(e.target.value) : e.target.value; } : null,
        onchange: creating ? null : function (e) { save(name, list ? e.target.value.split(",").map(str).filter(Boolean) : type === "number" ? num(e.target.value) : e.target.value); } }), wide);
    }
    function pick(name, label, options, shown) {
      var v = val(name) || "";
      if (!write) return v ? field(label, h("span", null, shown || v)) : null;
      return field(label, select(name, v, options, function (x) { save(name, x); if (creating) render(); }, label));
    }
    var epics = model.tickets.filter(function (e) { return e.is_epic && (!t || e.id !== t.id); });
    var epic = t && t.epic && model.byId[t.epic], sprint = t && model.sprints.filter(function (s) { return s.id === t.sprint; })[0], release = t && model.releases.filter(function (r) { return r.id === t.release; })[0];
    var people = {};
    P.members.forEach(function (m) { people[m] = 1; });
    model.tickets.forEach(function (x) { if (x.assignee) people[x.assignee] = 1; });

    var fields = h("div", { class: "pm-fields" },
      pick("status", "Status", P.statuses.map(function (s) { return [s.id, s.name]; }), t && statusPill(t)),
      pick("type", "Type", P.types.map(function (x) { return [x, x]; })),
      pick("priority", "Priority", P.priorities.map(function (x) { return [x, x]; })),
      text("assignee", "Assignee"),
      text("points", "Points", "number"),
      pick("sprint", "Sprint", [["", "None"]].concat(model.sprints.map(function (s) { return [s.id, s.name]; })), sprint && sprint.name),
      val("type") === "epic" ? null : pick("epic", "Epic", [["", "None"]].concat(epics.map(function (e) { return [e.id, e.key + " " + e.title]; })), epic && h("span", null, keyLink(epic), " " + epic.title)),
      pick("release", "Release", [["", "None"]].concat(model.releases.map(function (r) { return [r.id, r.name]; })), release && release.name),
      val("type") === "epic" ? text("start", "Start", "date") : null,
      text("due", "Due", "date"),
      text("labels", "Labels", "text", true),
      write ? text("blocked_by", "Blocked by (ticket ids)", "text", true) : null,
      h("datalist", { id: "pm-people" }, Object.keys(people).map(function (p) { return h("option", { value: p }); })));

    var description = write
      ? h("textarea", { rows: creating ? 5 : 7, "data-f": "description", placeholder: "What needs doing, and how you will know it is done.", value: val("description") || "",
          oninput: creating ? function (e) { d.description = e.target.value; } : null, onchange: creating ? null : function (e) { save("description", e.target.value); } })
      : (t.description ? h("div", { class: "pm-text" }, rich(t.description)) : h("p", { class: "pm-muted" }, "No description."));

    var body;
    if (creating) {
      body = [h("input", { class: "pm-title-input", type: "text", placeholder: "Title", value: d.title || "", "data-f": "title", oninput: function (e) { d.title = e.target.value; } }),
        fields, h("h4", null, "Description"), description,
        h("div", { class: "pm-actions" }, h("button", { onclick: function () {
          if (!str(d.title)) return toast("A ticket needs a title.");
          var draft = ui.draft;
          ui.draft = null;
          create(draft).then(function (id) { ui.open = id; render(); }).catch(function () { ui.draft = draft; render(); });
        } }, "Create ticket"), h("button", { class: "pm-quiet", onclick: close }, "Cancel"))];
    } else {
      var kids = model.tickets.filter(function (k) { return k.epic === t.id || k.parent === t.id; });
      var blocks = model.tickets.filter(function (k) { return k.blocked_by.indexOf(t.id) >= 0; });
      var parent = t.parent && model.byId[t.parent];
      var mentions = arr(hive && hive.thread && hive.thread.replies).filter(function (r) { return new RegExp("\\b" + t.key + "\\b", "i").test(str(r.body)); });
      var link = function (x) { return h("li", null, typeMark(x.type), " ", keyLink(x), " " + x.title + " ", statusPill(x)); };
      var draftComment = h("textarea", { rows: 2, placeholder: "Add a comment", "data-f": "comment" });
      body = [
        write ? h("input", { class: "pm-title-input", type: "text", value: t.title, "data-f": "title", onchange: function (e) { if (str(e.target.value)) save("title", e.target.value); } }) : h("h2", null, t.title),
        t.blocked ? h("p", { class: "pm-callout" }, t.blockers.length ? ["Blocked by ", t.blockers.map(function (b, i) { return [i ? ", " : "", keyLink(b)]; })] : "Flagged as blocked.") : null,
        fields, h("h4", null, "Description"), description,
        t.links.length ? [h("h4", null, "Links"), h("ul", { class: "pm-list" }, t.links.map(function (l) {
          var url = str(l.url);
          return h("li", null, /^https?:\/\//.test(url) ? h("a", { href: url, target: "_blank", rel: "noopener noreferrer" }, str(l.title) || url) : str(l.title) || url);
        }))] : null,
        parent ? [h("h4", null, "Parent"), h("ul", { class: "pm-list" }, link(parent))] : null,
        kids.length ? [h("h4", null, t.is_epic ? "Tickets in this epic" : "Sub-tasks"), bar(kids.filter(function (k) { return k.done; }).length, kids.length), h("ul", { class: "pm-list" }, kids.map(link))] : null,
        blocks.length ? [h("h4", null, "Blocks"), h("ul", { class: "pm-list" }, blocks.map(link))] : null,
        h("h4", null, "Comments"),
        t.comments.length ? h("ul", { class: "pm-comments" }, t.comments.map(function (c) {
          return h("li", null, h("div", { class: "pm-muted" }, avatar(str(c.by)), " " + str(c.by) + (c.at ? " · " + ago(c.at) : "")), h("div", { class: "pm-text" }, rich(str(c.text))));
        })) : h("p", { class: "pm-muted" }, "No comments."),
        write ? h("div", { class: "pm-form" }, draftComment, h("div", { class: "pm-actions" }, h("button", { onclick: function () { var v = draftComment.value; draftComment.value = ""; comment(t.id, v).catch(function () {}); } }, "Comment"))) : null,
        mentions.length ? [h("h4", null, "Mentioned in this thread's replies"), h("ul", { class: "pm-comments" }, mentions.map(function (r) {
          var b = str(r.body);
          return h("li", null, h("div", { class: "pm-muted" }, avatar(str(r.agent_name)), " " + str(r.agent_name) + " · " + ago(r.created_at)), h("div", { class: "pm-text" }, rich(b.length > 400 ? b.slice(0, 400) + "…" : b)));
        }))] : null,
        t.history.length ? h("details", null, h("summary", null, "History", h("span", { class: "pm-count" }, t.history.length)), h("ul", { class: "pm-list pm-muted" }, t.history.slice().reverse().map(function (x) {
          return h("li", null, str(x.by) + " changed " + str(x.field) + ": " + (x.from == null ? "none" : str(x.from)) + " → " + (x.to == null ? "none" : str(x.to)) + (x.at ? " · " + ago(x.at) : ""));
        }))) : null,
        h("p", { class: "pm-muted pm-foot" }, [t.reporter ? "Reported by " + t.reporter : "", t.created_at ? "created " + ago(t.created_at) : "", t.updated_by ? "last written by " + t.updated_by + " " + ago(t.updated_at) : "", t.archived ? "archived in " + t.src : "stored as ticket." + t.id].filter(Boolean).join(" · ")),
        write ? h("div", { class: "pm-actions" }, confirmButton("Delete ticket", "Delete " + t.key + " for good", function () { ui.open = null; removeTicket(t).catch(function () {}); render(); })) : null];
    }
    return h("div", { class: "pm-shade", onclick: function (e) { if (e.target === e.currentTarget) close(); } },
      h("aside", { class: "pm-detail", role: "dialog", "aria-label": creating ? "New ticket" : t.key, tabindex: "-1", onkeydown: function (e) { if (e.key === "Escape") close(); } },
        h("header", { class: "pm-detail-head" }, creating ? h("strong", null, "New ticket") : [typeMark(t.type), h("strong", null, t.key)], h("span", { class: "pm-spacer" }),
          h("button", { class: "pm-quiet", "aria-label": "Close", onclick: close }, "✕")),
        body));
  }

  // ---- drawing: the page ----
  var TABS = [["board", "Board"], ["backlog", "Backlog"], ["roadmap", "Roadmap"], ["reports", "Reports"]];
  function drawHead() {
    var P = model.project, people = {}, labels = {}, open = model.tickets.filter(function (t) { return !t.done && !t.is_epic; }).length;
    model.tickets.forEach(function (t) { if (t.assignee) people[t.assignee] = 1; t.labels.forEach(function (l) { labels[l] = 1; }); });
    var opts = function (all, set) { return [["", all]].concat(Object.keys(set).sort().map(function (k) { return [k, k]; })); };
    var sprint = activeSprint();
    return [
      h("div", { class: "pm-title" }, h("span", { class: "pm-badge" }, P.key), h("div", null, h("h1", null, P.name),
        h("div", { class: "pm-muted" }, [P.description, open + " open of " + model.tickets.filter(function (t) { return !t.is_epic; }).length + " tickets", sprint ? sprint.name + " in progress" : ""].filter(Boolean).join(" · "))),
        h("span", { class: "pm-spacer" }),
        canWrite() ? h("button", { class: "pm-primary", onclick: function () { ui.open = null; ui.draft = { type: "task", status: P.statuses[0].id, priority: "medium", sprint: boardScope() !== "all" && boardScope() !== "none" && ui.tab === "board" ? boardScope() : "" }; render(); } }, "New ticket") : null),
      h("div", { class: "pm-nav" },
        h("div", { class: "pm-tabs", role: "tablist" }, TABS.map(function (tab) {
          return h("button", { role: "tab", "aria-selected": ui.tab === tab[0] ? "true" : "false", "data-f": "tab-" + tab[0], onclick: function () { ui.tab = tab[0]; render(); } }, tab[1]);
        })),
        h("span", { class: "pm-spacer" }),
        h("input", { type: "search", class: "pm-search", placeholder: "Search tickets", "aria-label": "Search tickets", value: ui.q, oninput: function (e) { ui.q = e.target.value; drawMain(); } }),
        select("f-assignee", ui.assignee, opts("Everyone", people).concat([["-", "Unassigned"]]), function (v) { ui.assignee = v; render(); }, "Assignee"),
        select("f-type", ui.type, [["", "All types"]].concat(P.types.map(function (x) { return [x, x]; })), function (v) { ui.type = v; render(); }, "Type"),
        Object.keys(labels).length ? select("f-label", ui.label, opts("All labels", labels), function (v) { ui.label = v; render(); }, "Label") : null)];
  }
  function replace(el, kids) { while (el.firstChild) el.removeChild(el.firstChild); add(el, [kids]); }
  function drawMain() {
    var main = root.querySelector(".pm-main");
    replace(main, ui.tab === "backlog" ? drawBacklog() : ui.tab === "roadmap" ? drawRoadmap() : ui.tab === "reports" ? drawReports() : drawBoard());
    main.setAttribute("data-tab", ui.tab);
  }
  function typing() {
    var a = document.activeElement;
    return !!(root && a && root.contains(a) && (a.tagName === "TEXTAREA" || (a.tagName === "INPUT" && a.type !== "search")));
  }
  function render() {
    if (!root) return;
    stale = false;
    var a = document.activeElement, focus = a && root.contains(a) && a.getAttribute("data-f"), searching = a && a.classList && a.classList.contains("pm-search");
    var scroll = root.querySelector(".pm-detail");
    var top = scroll ? scroll.scrollTop : 0;
    if (!searching) replace(root.querySelector(".pm-head"), drawHead());
    drawMain();
    replace(root.querySelector(".pm-layer"), drawDetail());
    replace(root.querySelector(".pm-status-line"), [
      canWrite() ? "You can edit this board. " : "Agents keep this board through the hive's API. ",
      model.names + " of 100 data names used" + (model.names >= 80 ? ": archive finished tickets soon." : "."),
    ]);
    if (focus) { var again = root.querySelector('[data-f="' + focus + '"]'); if (again) again.focus(); }
    scroll = root.querySelector(".pm-detail");
    if (scroll) scroll.scrollTop = top;
    if (hive && hive.resize) hive.resize();
  }
  function refresh() {
    model = read(hive.data.all());
    watchers.forEach(function (fn) { try { fn(model); } catch (err) { setTimeout(function () { throw err; }); } });
    if (typing()) stale = true; else render();
  }

  function mount(el, options) {
    if (!el || root) return;
    options = options || {};
    root = el;
    if (TABS.some(function (t) { return t[0] === options.tab; })) ui.tab = options.tab;
    root.classList.add("pm");
    if (options.page !== false) document.documentElement.classList.add("pm-page");
    replace(root, [h("header", { class: "pm-head" }), h("main", { class: "pm-main" }), h("div", { class: "pm-layer" }),
      h("footer", { class: "pm-status-line pm-muted" }), h("div", { class: "pm-toast", role: "status", hidden: true })]);
    root.addEventListener("focusout", function () { setTimeout(function () { if (stale && !typing()) render(); }, 0); });
    render();
  }

  if (hive) hive.data.onChange(refresh);
  window.pm = {
    version: 1,
    mount: mount,
    read: read,
    model: function () { return model; },
    onChange: function (fn) { watchers.push(fn); },
    open: open,
    create: create,
    update: update,
    comment: comment,
  };
  document.addEventListener("DOMContentLoaded", function () {
    var el = document.querySelector("[data-pm]");
    if (el) mount(el, { tab: el.getAttribute("data-pm") });
  });
})();
