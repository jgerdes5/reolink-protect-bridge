"use strict";

// Theme choice: light, dark or system (default). Loaded synchronously in
// <head> so the theme is set before the first paint. The CSS reads
// <html data-theme>; without it, prefers-color-scheme decides.
(function () {
  const KEY = "rpb-theme";
  const root = document.documentElement;

  function stored() {
    try { return localStorage.getItem(KEY); } catch (_) { return null; }
  }

  function apply(choice) {
    if (choice !== "light" && choice !== "dark") choice = "system";
    if (choice === "system") root.removeAttribute("data-theme");
    else root.setAttribute("data-theme", choice);
    for (const b of document.querySelectorAll("[data-theme-choice]")) {
      b.setAttribute("aria-pressed", String(b.dataset.themeChoice === choice));
    }
    return choice;
  }

  apply(stored());

  document.addEventListener("DOMContentLoaded", () => {
    apply(stored());
    for (const b of document.querySelectorAll("[data-theme-choice]")) {
      b.addEventListener("click", () => {
        const choice = apply(b.dataset.themeChoice);
        try {
          if (choice === "system") localStorage.removeItem(KEY);
          else localStorage.setItem(KEY, choice);
        } catch (_) { /* storage blocked: the choice lasts until reload */ }
      });
    }
  });
})();
