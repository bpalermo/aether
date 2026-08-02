/*
 * Filters the proposals index in the page.
 *
 * Vanilla, no dependencies, no network: aethermesh.dev makes no third-party
 * requests, and a table of 35 rows does not need a library to hide some of them.
 * The rows are markdown the site generates from docs/proposals/*.md, so this
 * file knows nothing about the corpus — it reads the status off each row's dot
 * and searches the row's own text.
 *
 * With JavaScript off the page is still complete: every row is visible, and the
 * controls hide themselves (see .aether-filter in extra.css).
 */

(function () {
  "use strict";

  function ready(fn) {
    if (document.readyState !== "loading") {
      fn();
    } else {
      document.addEventListener("DOMContentLoaded", fn);
    }
  }

  ready(function () {
    var panel = document.querySelector(".aether-filter");
    var table = document.querySelector(".aether-proposals table");
    if (!panel || !table) {
      return;
    }

    var query = panel.querySelector(".aether-filter__q");
    var count = panel.querySelector(".aether-filter__count");
    var chips = Array.prototype.slice.call(panel.querySelectorAll(".aether-chip"));
    var rows = Array.prototype.slice.call(table.querySelectorAll("tbody tr"));

    // Read each row once: textContent includes the hidden haystack span, which
    // is where the slug and the Relates:/Supersedes:/Follows: refs live.
    var entries = rows.map(function (row) {
      var dot = row.querySelector(".aether-dot");
      var status = (dot && dot.getAttribute("data-status")) || "";
      if (status === "superseded") {
        row.classList.add("is-superseded");
      }
      return {
        row: row,
        status: status,
        text: (row.textContent || "").toLowerCase().replace(/\s+/g, " "),
      };
    });

    var status = "all";

    function apply() {
      var terms = (query.value || "")
        .toLowerCase()
        .split(/\s+/)
        .filter(Boolean);
      var shown = 0;

      entries.forEach(function (entry) {
        var visible =
          (status === "all" || entry.status === status) &&
          terms.every(function (term) {
            return entry.text.indexOf(term) !== -1;
          });
        entry.row.hidden = !visible;
        if (visible) {
          shown += 1;
        }
      });

      panel.classList.toggle("is-filtered", shown !== entries.length);
      count.textContent =
        shown === entries.length
          ? entries.length + " proposals"
          : shown + " of " + entries.length + " proposals";
    }

    chips.forEach(function (chip) {
      chip.addEventListener("click", function () {
        status = chip.getAttribute("data-status") || "all";
        chips.forEach(function (other) {
          other.classList.toggle("is-on", other === chip);
          other.setAttribute("aria-pressed", other === chip ? "true" : "false");
        });
        apply();
      });
    });

    query.addEventListener("input", apply);
    query.addEventListener("search", apply);

    // Escape clears, so the keyboard alone gets you back to the full list.
    query.addEventListener("keydown", function (event) {
      if (event.key === "Escape" && query.value) {
        event.stopPropagation();
        query.value = "";
        apply();
      }
    });

    panel.classList.add("is-live");
    apply();
  });
})();
