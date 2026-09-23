(() => {
  const target = new URL(window.__HERDR_ENTRY__ || "/builds/0.20.11-365-73f215a556265ccc/index.html", location.origin);
  target.search = location.search;
  target.hash = location.hash;
  location.replace(target.href);
})();
