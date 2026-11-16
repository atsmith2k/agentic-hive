// The hive page's own script. It answers the sandboxed frames that agents'
// views run in. A frame cannot reach the network or this page: every request
// it makes arrives here as a message, and only the operations below exist.
(function () {
  "use strict";

  function getJSON(url, options) {
    return fetch(url, Object.assign({ credentials: "same-origin", headers: { Accept: "application/json" } }, options)).then(function (res) {
      return res.json().then(function (body) {
        if (!res.ok) throw new Error(body.error || "request failed (" + res.status + ")");
        return body;
      });
    });
  }

  var drawer = document.getElementById("drawer");

  // fillForm puts values a view suggests into the hive's own form. The
  // person still reads them and presses Submit: a view cannot answer for them.
  function fillForm(values) {
    var form = document.querySelector(".respond-form");
    if (!form) throw new Error("this thread has no form you can answer");
    Object.keys(values).forEach(function (name) {
      var want = values[name];
      var list = Array.isArray(want) ? want.map(String) : null;
      Array.prototype.forEach.call(form.elements, function (el) {
        if (el.name !== name) return;
        if (el.type === "radio") el.checked = el.value === String(want);
        else if (el.type === "checkbox") el.checked = list ? list.indexOf(el.value) >= 0 : want === true || want === "true";
        else el.value = want == null ? "" : String(want);
      });
    });
    if (drawer) drawer.open = true;
    var section = document.getElementById("respond");
    section.classList.add("prefilled");
    section.scrollIntoView({ behavior: "smooth", block: "center" });
    return { filled: true };
  }

  function storeURL(thread, name) {
    return "/dashboard/threads/" + encodeURIComponent(thread) + "/store" + (name ? "/" + encodeURIComponent(name) : "");
  }

  var ops = {
    fetch: function (frame, args) {
      return getJSON(frame.dataset.fetch, {
        method: "POST",
        headers: { "Content-Type": "application/json", Accept: "application/json" },
        body: JSON.stringify({ url: String(args.url || "") }),
      });
    },
    context: function () { return getJSON("/dashboard/data/context"); },
    threads: function (frame, args) {
      var q = new URLSearchParams();
      ["q", "tag", "agent", "status"].forEach(function (k) { if (args[k]) q.set(k, String(args[k])); });
      return getJSON("/dashboard/data/threads?" + q.toString());
    },
    respond: function (frame, args) { return Promise.resolve(fillForm(args.values || {})); },
    "data.set": function (frame, args) {
      return getJSON(storeURL(frame.dataset.thread, args.name), {
        method: "PUT",
        headers: { "Content-Type": "application/json", Accept: "application/json" },
        body: JSON.stringify(args.value === undefined ? null : args.value),
      }).then(function (data) { return publish(frame.dataset.thread, data); });
    },
    "data.remove": function (frame, args) {
      return getJSON(storeURL(frame.dataset.thread, args.name), { method: "DELETE" })
        .then(function (data) { return publish(frame.dataset.thread, data); });
    },
  };

  var frames = Array.prototype.slice.call(document.querySelectorAll("iframe.hive-view, iframe.hive-card"));

  // A frame's document confines itself: it is served with a "sandbox"
  // policy, whatever frames it. The frame's sandbox attribute says the same
  // thing a second time. Some embedded browsers refuse to load any sandboxed
  // frame. There, and only there, the frame is loaded again without the
  // attribute, and the document's own policy still applies.
  function refused(frame) {
    var entries = performance.getEntriesByType("resource").filter(function (e) { return e.name === frame.src; });
    var last = entries[entries.length - 1];
    if (last && typeof last.responseStatus === "number") return last.responseStatus === 0;
    return true; // no way to tell: a frame that never started is treated as refused
  }
  frames.forEach(function (frame, i) {
    function check() {
      setTimeout(function () {
        var current = frames[i];
        if (current !== frame || frame.dataset.alive || !frame.hasAttribute("sandbox") || !refused(frame)) return;
        var plain = frame.cloneNode(false);
        plain.removeAttribute("sandbox");
        frame.parentNode.replaceChild(plain, frame);
        frames[i] = plain;
      }, 1200);
    }
    frame.addEventListener("load", check);
    setTimeout(check, 4000);
  });

  // The thread's data store is shared by every frame of that thread on the
  // page, and re-read every few seconds so a view follows what agents write.
  var stores = {};
  function publish(thread, data) {
    var text = JSON.stringify(data);
    if (stores[thread] !== text) {
      stores[thread] = text;
      frames.forEach(function (f) {
        if (f.dataset.thread === thread && f.dataset.fetch && f.contentWindow) f.contentWindow.postMessage({ hive: "data", data: data }, "*");
      });
    }
    return data;
  }
  var watched = {};
  frames.forEach(function (f) { if (f.dataset.fetch) watched[f.dataset.thread] = true; });
  setInterval(function () {
    if (document.hidden) return;
    Object.keys(watched).forEach(function (thread) {
      getJSON(storeURL(thread)).then(function (data) { publish(thread, data); }).catch(function () {});
    });
  }, 5000);

  window.addEventListener("message", function (e) {
    var frame = frames.filter(function (f) { return f.contentWindow === e.source; })[0];
    var m = e.data;
    if (!frame || !m) return;
    frame.dataset.alive = "1";
    if (m.hive === "size") {
      // The canvas and feed cards have their own size; a view set among text grows with its content.
      if (!frame.classList.contains("canvas-frame") && !frame.classList.contains("hive-card")) {
        frame.style.height = Math.max(120, Math.min(Number(m.height) || 0, 6000)) + "px";
      }
      return;
    }
    if (m.hive !== "call" || !ops.hasOwnProperty(m.op) || !frame.dataset.fetch) return;
    // The frame's origin is opaque, so "*" is the only target that reaches
    // it; e.source pins the reply to the frame that asked.
    Promise.resolve()
      .then(function () { return ops[m.op](frame, m.args || {}); })
      .then(function (result) { e.source.postMessage({ hive: "reply", id: m.id, result: result }, "*"); })
      .catch(function (err) { e.source.postMessage({ hive: "reply", id: m.id, error: String(err.message || err) }, "*"); });
  });

  // Escape closes the drawer.
  if (drawer) document.addEventListener("keydown", function (e) { if (e.key === "Escape") drawer.open = false; });

  // Tables drawn from hive:table blocks sort when a heading is clicked.
  Array.prototype.forEach.call(document.querySelectorAll("table[data-sortable]"), function (table) {
    var heads = table.querySelectorAll("thead th");
    Array.prototype.forEach.call(heads, function (th, col) {
      th.tabIndex = 0;
      th.setAttribute("role", "button");
      th.setAttribute("aria-sort", "none");
      function sort() {
        var asc = th.getAttribute("aria-sort") !== "ascending";
        Array.prototype.forEach.call(heads, function (h) { h.setAttribute("aria-sort", "none"); });
        th.setAttribute("aria-sort", asc ? "ascending" : "descending");
        var body = table.tBodies[0];
        var rows = Array.prototype.slice.call(body.rows);
        var key = function (row) { return row.cells[col].textContent.trim(); };
        var numeric = rows.every(function (r) { return key(r) === "" || !isNaN(parseFloat(key(r).replace(/[,%$#]/g, ""))); });
        rows.sort(function (a, b) {
          var x = key(a), y = key(b);
          var d = numeric ? (parseFloat(x.replace(/[,%$#]/g, "")) || 0) - (parseFloat(y.replace(/[,%$#]/g, "")) || 0) : x.localeCompare(y);
          return asc ? d : -d;
        });
        rows.forEach(function (r) { body.appendChild(r); });
      }
      th.addEventListener("click", sort);
      th.addEventListener("keydown", function (e) { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); sort(); } });
    });
  });
})();
