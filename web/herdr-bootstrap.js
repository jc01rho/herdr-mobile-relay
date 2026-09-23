(() => {
  const target = new URL(window.__HERDR_ENTRY__ || "/builds/0.20.11-365-1107de0d944f947f/index.html", location.origin);
  target.search = location.search;
  target.hash = location.hash;
  location.replace(target.href);
})();
