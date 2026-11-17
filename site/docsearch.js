/* Docs search. The index is built by pagefind when the site image is built
   (site/Dockerfile, stage "search") and served from /pagefind/ on this site:
   no search service, no request anywhere else. The form ships with the
   `hidden` attribute, so without JavaScript — or where the index is missing,
   as when the pages are opened straight from the repository — nothing shows
   and the page list below it is the way around. */
(function () {
  "use strict";
  var form = document.querySelector("form.docsearch");
  if (!form || !("noModule" in HTMLScriptElement.prototype)) return; // no import()
  var input = form.querySelector("input[type=search]");
  var list = form.querySelector(".docsearch-results");
  var status = form.querySelector(".docsearch-status");
  var pagefind = null, loading = null, seq = 0;

  form.hidden = false;
  form.addEventListener("submit", function (e) { e.preventDefault(); });

  function load() {
    if (!loading) {
      loading = import("/pagefind/pagefind.js").then(function (pf) {
        pagefind = pf;
        return pf.init();
      }).catch(function () {
        form.hidden = true; // no index here: leave the page list alone
      });
    }
    return loading;
  }

  // /docs/exec.html → /docs/exec, /docs.html → /docs: the URLs nginx serves.
  function cleanURL(u) {
    return u.replace(/\.html(?=$|[#?])/, "");
  }

  function show(results, q) {
    list.textContent = "";
    results.forEach(function (r) {
      var li = document.createElement("li");
      var a = document.createElement("a");
      a.href = cleanURL(r.url);
      a.textContent = (r.meta && r.meta.title) || r.url;
      var p = document.createElement("p");
      p.innerHTML = r.excerpt; // pagefind escapes the text and adds only <mark>
      li.appendChild(a);
      li.appendChild(p);
      list.appendChild(li);
    });
    status.textContent = q === "" ? "" :
      results.length === 0 ? "No page matches “" + q + "”." :
      results.length + (results.length === 1 ? " page" : " pages") + " match.";
  }

  function run() {
    var q = input.value.trim();
    var mine = ++seq;
    if (q === "") { show([], ""); return; }
    load().then(function () {
      if (!pagefind) return null;
      return pagefind.debouncedSearch(q, {}, 150);
    }).then(function (search) {
      if (!search || mine !== seq) return null; // superseded by a newer query
      return Promise.all(search.results.slice(0, 8).map(function (r) { return r.data(); }));
    }).then(function (results) {
      if (results && mine === seq) show(results, q);
    });
  }

  // ?q=… opens a page with that search done: a search can be linked.
  var q0 = new URLSearchParams(location.search).get("q");
  if (q0) { input.value = q0; run(); }

  input.addEventListener("focus", load, { once: true });
  input.addEventListener("input", run);
  input.addEventListener("keydown", function (e) {
    if (e.key === "Escape" && input.value !== "") { input.value = ""; run(); }
  });
})();
