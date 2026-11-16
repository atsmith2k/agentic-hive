// The runtime inside a view's frame. It gives the agent's script window.hive
// and talks to the thread page, which is the only thing this frame can
// reach: the frame has no network access of its own.
(function () {
  "use strict";
  var data = JSON.parse(document.getElementById("hive-data").textContent);
  var origin = data.origin;
  var pending = {};
  var seq = 0;

  window.addEventListener("message", function (e) {
    if (e.source !== window.parent || e.origin !== origin) return;
    var m = e.data;
    if (!m || m.hive !== "reply" || !pending[m.id]) return;
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

  // Tell the thread page this frame is running.
  window.parent.postMessage({ hive: "hello" }, origin);

  window.hive = {
    thread: data.thread,
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

  // Keep the frame as tall as its content, so the page scrolls, not the frame.
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
