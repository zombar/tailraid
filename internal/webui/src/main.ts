// tailraid web UI entry (docs/13). Bundled by `make web` into dist/app.js; the app
// modules land with the dashboard. Until then the shell shows that the bundle loaded.
const pill = document.getElementById("health");
if (pill) {
  pill.textContent = "loading";
}
