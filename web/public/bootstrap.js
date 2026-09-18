(() => {
  const param = new URLSearchParams(window.location.search).get("scoutTheme");
  const theme =
    param || (window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light");
  document.documentElement.setAttribute("data-theme", theme);
})();

(() => {
  let secret = window.location.hash.slice(1);
  if (window.location.hash) {
    window.history.replaceState(null, "", window.location.pathname + window.location.search);
  }
  window.__takeUpfileSecret = () => {
    const value = secret;
    secret = "";
    delete window.__takeUpfileSecret;
    return value;
  };
  const preference = window.matchMedia("(prefers-color-scheme: dark)");
  preference.addEventListener("change", (event) => {
    if (!new URLSearchParams(window.location.search).has("scoutTheme")) {
      document.documentElement.setAttribute("data-theme", event.matches ? "dark" : "light");
    }
  });
})();
