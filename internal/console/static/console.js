// Optional progressive enhancement (< 3 KB): theme toggle and aria-busy while loading.
(function () {
  var root = document.documentElement;
  try {
    var saved = localStorage.getItem("mcpgw-theme");
    if (saved === "light" || saved === "dark") root.setAttribute("data-theme", saved);
  } catch (e) {}
  document.addEventListener("DOMContentLoaded", function () {
    var btn = document.getElementById("theme-toggle");
    if (btn) {
      btn.hidden = false;
      btn.addEventListener("click", function () {
        var dark = root.getAttribute("data-theme") === "dark" ||
          (!root.getAttribute("data-theme") && window.matchMedia("(prefers-color-scheme: dark)").matches);
        var next = dark ? "light" : "dark";
        root.setAttribute("data-theme", next);
        btn.setAttribute("aria-pressed", String(next === "dark"));
        try { localStorage.setItem("mcpgw-theme", next); } catch (e) {}
      });
    }
    var main = document.querySelector("main");
    function busy() { if (main) main.setAttribute("aria-busy", "true"); }
    document.querySelectorAll("form").forEach(function (f) { f.addEventListener("submit", busy); });
    document.querySelectorAll("nav.main a, a.pagelink").forEach(function (a) { a.addEventListener("click", busy); });
    window.addEventListener("pageshow", function () { if (main) main.setAttribute("aria-busy", "false"); });
  });
})();
