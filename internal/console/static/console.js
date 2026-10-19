// Optional progressive enhancement (< 3 KB): theme toggle, loading state and focus on results.
(function () {
  var root = document.documentElement;
  try {
    var saved = localStorage.getItem("mcpgw-theme");
    if (saved === "light" || saved === "dark") root.setAttribute("data-theme", saved);
  } catch (e) {}
  function isDark() {
    var t = root.getAttribute("data-theme");
    return t === "dark" || (!t && window.matchMedia("(prefers-color-scheme: dark)").matches);
  }
  document.addEventListener("DOMContentLoaded", function () {
    var btn = document.getElementById("theme-toggle");
    if (btn) {
      btn.hidden = false;
      btn.setAttribute("aria-pressed", String(isDark()));
      btn.addEventListener("click", function () {
        var next = isDark() ? "light" : "dark";
        root.setAttribute("data-theme", next);
        btn.setAttribute("aria-pressed", String(next === "dark"));
        try { localStorage.setItem("mcpgw-theme", next); } catch (e) {}
      });
    }
    var main = document.querySelector("main");
    var loading = document.getElementById("loading");
    function busy(on) {
      if (main) main.setAttribute("aria-busy", String(on));
      if (loading) loading.hidden = !on;
    }
    document.querySelectorAll("form").forEach(function (f) {
      f.addEventListener("submit", function () { busy(true); });
    });
    document.querySelectorAll("main a[href^='/console'], nav a").forEach(function (a) {
      a.addEventListener("click", function (ev) {
        if (ev.button !== 0 || ev.metaKey || ev.ctrlKey || ev.shiftKey || ev.altKey || a.target) return;
        busy(true);
      });
    });
    window.addEventListener("pageshow", function () { busy(false); });
    // A navigation cancelled with Esc never fires pageshow: clear the state when the page is shown.
    document.addEventListener("visibilitychange", function () { if (!document.hidden) busy(false); });
    document.addEventListener("keydown", function (ev) { if (ev.key === "Escape") busy(false); });
    var target = document.getElementById("error") || document.getElementById("flash") || document.getElementById("result");
    if (target) target.focus();
  });
})();
