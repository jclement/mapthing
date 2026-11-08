// Apply the saved theme before first paint (no flash). External file so a strict CSP works.
(() => {
  try {
    const t = localStorage.getItem("mapthing.theme");
    const dark =
      t === "dark" || (t !== "light" && matchMedia("(prefers-color-scheme: dark)").matches);
    if (dark) document.documentElement.classList.add("dark");
  } catch {
    /* storage unavailable */
  }
})();
