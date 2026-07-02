// metalkit operator UI — Slice E: machine list filter + sort overlay.
// Self-contained: hooks into the existing list page by observing the tbody
// that app.js populates. Does not touch app.js or other slices.

(function () {
  "use strict";
  if (document.body.dataset.page !== "list") return;

  // Column index map (must match index.html thead order):
  //   0 Status, 1 Serial, 2 Manufacturer/Product, 3 UUID,
  //   4 BMC IP, 5 Managed, 6 Last seen, 7 Actions
  var COL = { STATUS: 0, SERIAL: 1, PRODUCT: 2, UUID: 3, BMC: 4, MANAGED: 5, LAST_SEEN: 6 };
  var SORTABLE = {
    status:    { col: COL.STATUS,    type: "status" },
    serial:    { col: COL.SERIAL,    type: "string" },
    product:   { col: COL.PRODUCT,   type: "string" },
    bmc:       { col: COL.BMC,       type: "string" },
    last_seen: { col: COL.LAST_SEEN, type: "date"   }
  };

  // Filter state mirrored in the URL query string.
  var state = {
    q: "",
    statuses: Object.create(null), // set of active status names
    sortKey: "last_seen",
    sortDir: "desc"
  };

  var debounceTimer = null;
  var barUnhidden = false;

  document.addEventListener("DOMContentLoaded", function () {
    parseURL();
    wireInputs();
    wireHeaders();
    syncControlsToState();

    var tbody = document.getElementById("machines-tbody");
    if (!tbody) return;
    var observer = new MutationObserver(function () {
      if (!barUnhidden) {
        var bar = document.getElementById("machine-filter-bar");
        if (bar) bar.hidden = false;
        barUnhidden = true;
      }
      // sortRows mutates tbody (appendChild), which would re-trigger this
      // observer and loop forever. Disconnect during applyAll, reconnect
      // after. Using microtask to avoid taking a snapshot of mid-mutation.
      observer.disconnect();
      try {
        applyAll();
      } finally {
        queueMicrotask(function () {
          observer.observe(tbody, { childList: true });
        });
      }
    });
    observer.observe(tbody, { childList: true });
  });

  // ---- URL <-> state ---------------------------------------------------

  function parseURL() {
    var p = new URLSearchParams(window.location.search);
    state.q = p.get("q") || "";
    var st = p.get("status");
    if (st) {
      st.split(",").forEach(function (s) {
        s = s.trim().toLowerCase();
        if (s) state.statuses[s] = true;
      });
    }
    var sk = p.get("sort");
    if (sk && SORTABLE[sk]) state.sortKey = sk;
    var dir = p.get("dir");
    if (dir === "asc" || dir === "desc") state.sortDir = dir;
  }

  function writeURL() {
    var p = new URLSearchParams();
    if (state.q) p.set("q", state.q);
    var sts = Object.keys(state.statuses);
    if (sts.length > 0) p.set("status", sts.join(","));
    // Always include sort so the URL is self-describing.
    p.set("sort", state.sortKey);
    p.set("dir", state.sortDir);
    var qs = p.toString();
    var url = window.location.pathname + (qs ? "?" + qs : "");
    window.history.replaceState({}, "", url);
  }

  // ---- DOM controls ----------------------------------------------------

  function syncControlsToState() {
    var input = document.getElementById("m-filter-text");
    if (input) input.value = state.q;
    document.querySelectorAll("#m-filter-status .chip").forEach(function (chip) {
      var s = chip.getAttribute("data-status");
      chip.classList.toggle("active", !!state.statuses[s]);
    });
  }

  function wireInputs() {
    var input = document.getElementById("m-filter-text");
    if (input) {
      input.addEventListener("input", function () {
        if (debounceTimer) clearTimeout(debounceTimer);
        debounceTimer = setTimeout(function () {
          state.q = input.value || "";
          writeURL();
          applyAll();
        }, 200);
      });
    }

    document.querySelectorAll("#m-filter-status .chip").forEach(function (chip) {
      chip.addEventListener("click", function () {
        var s = chip.getAttribute("data-status");
        if (!s) return;
        if (state.statuses[s]) delete state.statuses[s];
        else state.statuses[s] = true;
        chip.classList.toggle("active", !!state.statuses[s]);
        writeURL();
        applyAll();
      });
    });

    var clear = document.getElementById("m-filter-clear");
    if (clear) {
      clear.addEventListener("click", function () {
        state.q = "";
        state.statuses = Object.create(null);
        if (input) input.value = "";
        syncControlsToState();
        writeURL();
        applyAll();
      });
    }
  }

  function wireHeaders() {
    var table = document.getElementById("machines-table");
    if (!table) return;
    var ths = table.querySelectorAll("thead th");
    var headerKey = [null, null, null, null, null, null];
    headerKey[COL.STATUS] = "status";
    headerKey[COL.SERIAL] = "serial";
    headerKey[COL.PRODUCT] = "product";
    headerKey[COL.LAST_SEEN] = "last_seen";

    ths.forEach(function (th, idx) {
      var key = headerKey[idx];
      if (!key) return;
      th.classList.add("sortable");
      th.style.cursor = "pointer";
      th.dataset.sortKey = key;
      th.addEventListener("click", function () {
        if (state.sortKey === key) {
          state.sortDir = state.sortDir === "asc" ? "desc" : "asc";
        } else {
          state.sortKey = key;
          state.sortDir = key === "last_seen" ? "desc" : "asc";
        }
        writeURL();
        applyAll();
      });
    });
  }

  function paintArrows() {
    var table = document.getElementById("machines-table");
    if (!table) return;
    var ths = table.querySelectorAll("thead th");
    ths.forEach(function (th) {
      if (!th.dataset.sortKey) return;
      // Strip any prior arrow we added.
      var base = th.textContent.replace(/\s*[▲▼]\s*$/u, "");
      if (th.dataset.sortKey === state.sortKey) {
        th.textContent = base + (state.sortDir === "asc" ? " ▲" : " ▼");
      } else {
        th.textContent = base;
      }
    });
  }

  // ---- row data extraction --------------------------------------------

  function rowStatus(tr) {
    var dot = tr.querySelector(".status-dot[data-status]");
    if (dot) return (dot.getAttribute("data-status") || "").toLowerCase();
    return "";
  }

  function rowText(tr, colIdx) {
    var cell = tr.cells && tr.cells[colIdx];
    return cell ? cell.textContent.trim() : "";
  }

  function rowUUID(tr) {
    var cell = tr.cells && tr.cells[COL.UUID];
    if (!cell) return "";
    var span = cell.querySelector(".copyable[data-copy]");
    if (span) return (span.getAttribute("data-copy") || "").toLowerCase();
    return cell.textContent.trim().toLowerCase();
  }

  // last_seen comes from the cell's title attribute (set via fmtAbsolute,
  // e.g. "2026-05-25 12:34:56Z"). Parse to a unix timestamp; missing/invalid
  // sorts as 0 so it falls to the bottom of a desc sort.
  function rowLastSeenTS(tr) {
    var cell = tr.cells && tr.cells[COL.LAST_SEEN];
    if (!cell) return 0;
    var title = cell.getAttribute("title") || "";
    if (!title || title === "-") return 0;
    var t = Date.parse(title.replace(" ", "T"));
    return isNaN(t) ? 0 : t;
  }

  function matchesFilters(tr) {
    // status filter
    var activeStatuses = Object.keys(state.statuses);
    if (activeStatuses.length > 0) {
      var st = rowStatus(tr);
      if (!state.statuses[st]) return false;
    }
    // text filter across serial / product / uuid
    var q = state.q.trim().toLowerCase();
    if (!q) return true;
    var hay = (
      rowText(tr, COL.SERIAL) + " " +
      rowText(tr, COL.PRODUCT) + " " +
      rowUUID(tr) + " " +
      rowText(tr, COL.BMC)
    ).toLowerCase();
    return hay.indexOf(q) !== -1;
  }

  // ---- sort ------------------------------------------------------------

  function compareRows(a, b) {
    var spec = SORTABLE[state.sortKey];
    if (!spec) return 0;
    var av, bv, cmp;
    if (spec.type === "date") {
      av = rowLastSeenTS(a);
      bv = rowLastSeenTS(b);
      cmp = av - bv;
    } else if (spec.type === "status") {
      av = rowStatus(a);
      bv = rowStatus(b);
      cmp = av.localeCompare(bv);
    } else {
      av = rowText(a, spec.col).toLowerCase();
      bv = rowText(b, spec.col).toLowerCase();
      cmp = av.localeCompare(bv);
    }
    if (cmp === 0) {
      // stable secondary: uuid asc
      cmp = rowUUID(a).localeCompare(rowUUID(b));
    }
    return state.sortDir === "asc" ? cmp : -cmp;
  }

  function sortRows(tbody) {
    var rows = Array.prototype.slice.call(tbody.querySelectorAll("tr"));
    if (rows.length < 2) return;
    rows.sort(compareRows);
    var frag = document.createDocumentFragment();
    rows.forEach(function (r) { frag.appendChild(r); });
    tbody.appendChild(frag);
  }

  // ---- apply -----------------------------------------------------------

  function applyAll() {
    var tbody = document.getElementById("machines-tbody");
    if (!tbody) return;
    sortRows(tbody);

    var rows = tbody.querySelectorAll("tr");
    var total = rows.length;
    var shown = 0;
    rows.forEach(function (tr) {
      if (matchesFilters(tr)) {
        tr.style.display = "";
        shown++;
      } else {
        tr.style.display = "none";
      }
    });

    var count = document.getElementById("m-filter-count");
    if (count) {
      if (total === 0) count.textContent = "";
      else if (shown === total) count.textContent = "显示 " + total + " 条";
      else count.textContent = "显示 " + shown + " / " + total + " 条";
    }
    paintArrows();
  }
})();
