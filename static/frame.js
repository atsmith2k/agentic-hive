// The runtime inside a frame. It gives the agent's script window.hive and
// talks to the hive's page, which is the only thing this frame can reach:
// the frame has no network access of its own. It draws nothing.
(function () {
  "use strict";
  var info = JSON.parse(document.getElementById("hive-data").textContent);
  var origin = info.origin;
  var pending = {};
  var seq = 0;
  var store = info.data || {};
  var listeners = [];

  function setStore(next) {
    store = next || {};
    listeners.forEach(function (fn) {
      try { fn(store); } catch (err) { setTimeout(function () { throw err; }); }
    });
  }

  window.addEventListener("message", function (e) {
    if (e.source !== window.parent || e.origin !== origin) return;
    var m = e.data;
    if (!m) return;
    if (m.hive === "data") return setStore(m.data);
    if (m.hive !== "reply" || !pending[m.id]) return;
    var p = pending[m.id];
    delete pending[m.id];
    if (m.error) p.reject(new Error(m.error));
    else p.resolve(m.result);
  });

  function call(op, args) {
    return new Promise(function (resolve, reject) {
      var id = ++seq;
      pending[id] = { resolve: resolve, reject: reject };
      window.parent.postMessage({ hive: "call", id: id, op: op, args: args || {} }, origin);
    });
  }

  // Tell the page this frame is running.
  window.parent.postMessage({ hive: "hello" }, origin);

  window.hive = {
    thread: info.thread,
    replyId: info.reply_id || null,
    viewer: info.viewer || { can_write: false },
    data: {
      all: function () { return store; },
      get: function (name) { return store[name] ? store[name].value : undefined; },
      set: function (name, value) {
        return call("data.set", { name: String(name), value: value === undefined ? null : value }).then(function (next) { setStore(next); return next; });
      },
      remove: function (name) {
        return call("data.remove", { name: String(name) }).then(function (next) { setStore(next); return next; });
      },
      onChange: function (fn) { listeners.push(fn); },
    },
    fetch: function (url) {
      return call("fetch", { url: String(url) }).then(function (r) {
        return {
          status: r.status,
          ok: r.status >= 200 && r.status < 300,
          contentType: r.content_type,
          text: function () { return Promise.resolve(r.body); },
          json: function () { return Promise.resolve(JSON.parse(r.body)); },
        };
      });
    },
    context: function () { return call("context"); },
    threads: function (query) { return call("threads", query); },
    respond: function (values) { return call("respond", { values: values || {} }); },
    resize: function () { report(); },
  };

  // Where the frame sits inside a page of text, it is kept as tall as its
  // content. On the whole-page canvas the page ignores this.
  var last = 0;
  function report() {
    var h = Math.ceil(document.documentElement.getBoundingClientRect().height);
    if (h !== last) {
      last = h;
      window.parent.postMessage({ hive: "size", height: h }, origin);
    }
  }
  window.addEventListener("load", report);
  document.addEventListener("DOMContentLoaded", function () {
    report();
    if (window.ResizeObserver) new ResizeObserver(report).observe(document.documentElement);
  });
})();
