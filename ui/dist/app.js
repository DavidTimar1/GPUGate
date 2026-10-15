"use strict";

/* GPUGate console. No call here ever spends money except the Launch buttons, and those are
 * pressed by a person.
 *
 * The views (Machines, Projects, Sessions) sit behind a left menu, a console log in a right
 * drawer, one fixed message overlay at the top for every error and notice, one confirm sheet. The
 * routes the page calls are registered in internal/console/server.go.
 *
 * The model column recalculates by itself on any change to the model, context or chats
 * (markStaleAndRecalculate); there is no button for it. Success is shown by the screen changing,
 * never by a notice: showMsg is for errors and for things a person must read.
 *
 * Launch: a click on a row (or its Rent / Why not button) opens one panel under it. Pressing the
 * panel's Launch first asks GET /api/launch-offer and, when a cheaper machine (auto-move on for
 * the project) or one with fewer cards of the same card can serve the same launch, shows that
 * choice instead of sending the launch (offerHTML). Nothing is rented until a launch button is
 * pressed. Context, chats, model settings and max batched tokens are read from "What to run";
 * project, where and the idle limit from the panel.
 *
 * Everything that builds or starts the page runs from init() at the end of this file.
 */

var THEME_KEY = "gg-console-theme";
// The button shows what pressing it switches to: a sun while dark, a moon while light.
var THEME_ICON_SUN = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/></svg>';
var THEME_ICON_MOON = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M20 14.5A8 8 0 0 1 9.5 4 8 8 0 1 0 20 14.5z"/></svg>';
var X_ICON = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" aria-hidden="true"><path d="M6 6l12 12M18 6L6 18"/></svg>';

// ---------------------------------------------------------------------------------------------
// Login. Any answer of 401 means the session is gone (a restart, a password reset, thirty days):
// the browser goes to /login at once, and from then on no call is made and no error is drawn, so
// the 5 and 30 second polls do nothing while the page waits to leave.
// ---------------------------------------------------------------------------------------------
var sessionEnded = false;
function endSession() {
  sessionEnded = true;
  window.location.replace("/login");
}
(function () {
  var realFetch = window.fetch.bind(window);
  window.fetch = function () {
    if (sessionEnded) return new Promise(function () {});
    return realFetch.apply(null, arguments).then(function (response) {
      if (response.status === 401) {
        endSession();
        return new Promise(function () {});
      }
      return response;
    });
  };
})();
document.addEventListener("htmx:beforeRequest", function (ev) {
  if (sessionEnded) ev.preventDefault();
}, true);
document.addEventListener("htmx:beforeSwap", function (ev) {
  if (ev.detail.xhr && ev.detail.xhr.status === 401) {
    ev.detail.shouldSwap = false;
    ev.detail.isError = false;
    endSession();
  }
}, true);

function $(selector, root) { return (root || document).querySelector(selector); }
function $$(selector, root) { return Array.prototype.slice.call((root || document).querySelectorAll(selector)); }
function byId(id) { return document.getElementById(id); }

function escapeHtml(text) {
  var div = document.createElement("div");
  div.textContent = String(text == null ? "" : text);
  return div.innerHTML;
}

// ---------------------------------------------------------------------------------------------
// Theme: an icon button, the choice remembered per browser, the OS setting by default.
// ---------------------------------------------------------------------------------------------
function isDark() {
  var chosen = document.documentElement.getAttribute("data-theme");
  return chosen ? chosen === "dark" : window.matchMedia("(prefers-color-scheme: dark)").matches;
}
function paintThemeButton() {
  var dark = isDark();
  var btn = byId("theme-toggle");
  btn.setAttribute("aria-label", dark ? "Switch to light mode" : "Switch to dark mode");
  btn.innerHTML = dark ? THEME_ICON_SUN : THEME_ICON_MOON;
}
function initTheme() {
  var saved = null;
  try { saved = localStorage.getItem(THEME_KEY); } catch (e) { saved = null; }
  if (saved) document.documentElement.setAttribute("data-theme", saved);
  byId("theme-toggle").addEventListener("click", function () {
    var next = isDark() ? "light" : "dark";
    document.documentElement.setAttribute("data-theme", next);
    try { localStorage.setItem(THEME_KEY, next); } catch (e) {}
    paintThemeButton();
  });
  paintThemeButton();
}

// PROJECT_KEY remembers the last project chosen on a launch panel, per browser.
var PROJECT_KEY = "gg-console-project";
// The project every install starts with. The server never deletes it, so the page offers no
// Delete for it and falls back to it when no remembered project exists.
var DEFAULT_PROJECT = "coding";

var state = {
  loaded: false, showAll: false, shapes: [], models: [], projects: [], availabilityKnown: false,
  // stale: the model column does not match the boxes (a recalculation is pending or failed).
  // recalcFailed: the last recalculation was refused, so the column still holds old figures.
  stale: false, recalcFailed: false, loadFailed: false,
  // The table opens cheapest first: the whole machine's hourly price, ascending. Clicking a
  // header still cycles ascending, descending, then the order the answer arrived in.
  sort: { column: "cost", direction: "asc" },
  filters: { provider: "", availableOnly: false, hasModelOnly: false },
  search: ""
};

// ---------------------------------------------------------------------------------------------
// The one message overlay. Every error and every notice goes here, each with a small close
// button, and stays until closed. A message that describes a condition the strip polls (a
// provider that could not be asked) is withdrawn when the condition clears, and stays closed
// after a person closed it for as long as the condition lasts.
// ---------------------------------------------------------------------------------------------
var dismissedPoll = {};

function showMsg(text, kind, isPoll) {
  var box = byId("msgs");
  var exists = $$(".msg", box).some(function (m) { return m.dataset.t === text; });
  if (exists) return;
  var d = document.createElement("div");
  d.className = "msg" + (kind === "warn" ? " warn" : "");
  d.dataset.t = text;
  if (isPoll) d.dataset.poll = "1";
  d.innerHTML = '<p><span class="k">' + (kind === "warn" ? "Notice. " : "Error. ") + "</span>" +
    escapeHtml(text) + '</p><button class="x" type="button" aria-label="Close this message">' + X_ICON + "</button>";
  box.appendChild(d);
}

byId("msgs").addEventListener("click", function (ev) {
  var x = ev.target.closest(".x");
  if (!x) return;
  var msg = x.closest(".msg");
  if (msg.dataset.poll) dismissedPoll[msg.dataset.t] = true;
  msg.remove();
});

// syncPollMessages shows the strip's current "could not ask" sentences and withdraws the ones
// that no longer apply.
function syncPollMessages(texts) {
  Object.keys(dismissedPoll).forEach(function (t) {
    if (texts.indexOf(t) === -1) delete dismissedPoll[t];
  });
  $$(".msg[data-poll]", byId("msgs")).forEach(function (m) {
    if (texts.indexOf(m.dataset.t) === -1) m.remove();
  });
  texts.forEach(function (t) {
    if (!dismissedPoll[t]) showMsg(t, "warn", true);
  });
}

// ---------------------------------------------------------------------------------------------
// Views, the left menu (a drawer on a phone, a rail on a wide screen) and the console drawer.
// ---------------------------------------------------------------------------------------------
var wide = window.matchMedia("(min-width:60rem)");
var navEl = byId("nav");
var mainEl = byId("main");
var topbarEl = $(".topbar");

function setNav(open) {
  navEl.classList.toggle("open", open);
  byId("navscrim").hidden = !open;
  byId("ham").setAttribute("aria-expanded", String(open));
  mainEl.inert = open && !wide.matches;
  navEl.inert = !open && !wide.matches;
  if (open) byId("navclose").focus();
}
function syncNav() {
  navEl.inert = !wide.matches && !navEl.classList.contains("open");
  if (wide.matches) {
    navEl.classList.remove("open");
    byId("navscrim").hidden = true;
    mainEl.inert = false;
  }
}
wide.addEventListener("change", syncNav);
byId("ham").addEventListener("click", function () { setNav(true); });
byId("navclose").addEventListener("click", function () { setNav(false); byId("ham").focus(); });
byId("navscrim").addEventListener("click", function () { setNav(false); byId("ham").focus(); });

function showView(name) {
  ["machines", "projects", "sessions"].forEach(function (n) { byId("v-" + n).hidden = n !== name; });
  $$(".nav a").forEach(function (a) {
    if (a.dataset.view === name) a.setAttribute("aria-current", "page"); else a.removeAttribute("aria-current");
  });
  document.title = "GPUGate: " + name.charAt(0).toUpperCase() + name.slice(1);
}
function route() {
  var name = (location.hash || "#machines").slice(1);
  if (!byId("v-" + name)) name = "machines";
  showView(name);
  if (navEl.classList.contains("open")) setNav(false);
}
window.addEventListener("hashchange", function () {
  route();
  mainEl.focus({ preventScroll: true });
  window.scrollTo(0, 0);
});
byId("statuspill").addEventListener("click", function () {
  location.hash = "#machines";
  setTimeout(function () {
    byId("running").scrollIntoView();
    byId("run-h").focus();
  }, 30);
});

// The "i" buttons expand their note in place.
document.addEventListener("click", function (ev) {
  var b = ev.target.closest(".info");
  if (!b || b.id === "expall") return;
  var note = byId(b.getAttribute("aria-controls"));
  var open = b.getAttribute("aria-expanded") !== "true";
  b.setAttribute("aria-expanded", String(open));
  if (note) note.hidden = !open;
});

// ---------------------------------------------------------------------------------------------
// The confirm sheet: Delete machine, Delete project, Clear list. `run` does the work and answers
// an error sentence, or null; the sheet stays open and disabled while it runs. Cancelling or
// dismissing the sheet asks the server nothing.
// ---------------------------------------------------------------------------------------------
var modalBg = byId("modal-bg");
var modalOk = byId("modal-confirm");
var modalCancel = byId("modal-cancel");
var modalReturnFocus = null;
var modalRun = null;
var modalLabel = "";

function confirmBox(o) {
  modalReturnFocus = o.from || null;
  byId("modal-title").textContent = o.t;
  byId("modal-desc").textContent = o.d;
  byId("modal-target").textContent = o.tgt;
  modalLabel = o.ok;
  modalOk.textContent = o.ok;
  modalOk.disabled = false;
  modalRun = o.run;
  modalBg.hidden = false;
  [mainEl, topbarEl, navEl].forEach(function (x) { x.inert = true; });
  modalCancel.focus();
}
function closeConfirm() {
  modalBg.hidden = true;
  modalRun = null;
  mainEl.inert = false;
  topbarEl.inert = false;
  syncNav();
  if (modalReturnFocus && document.body.contains(modalReturnFocus)) modalReturnFocus.focus();
}
modalCancel.addEventListener("click", closeConfirm);
modalBg.addEventListener("click", function (ev) { if (ev.target === modalBg) closeConfirm(); });
modalOk.addEventListener("click", async function () {
  if (!modalRun) return;
  var run = modalRun;
  modalOk.disabled = true;
  modalOk.textContent = "Working\u2026";
  var error = null;
  try { error = await run(); } catch (e) { error = "Could not reach the server."; }
  modalOk.textContent = modalLabel;
  closeConfirm();
  if (error) showMsg(error);
});

// ---------------------------------------------------------------------------------------------
// Sorting, filtering, search.
// ---------------------------------------------------------------------------------------------

// One column's own value out of one row, for sorting and nothing else. `null` means the row
// carries no value for that column, and the sort below always puts those rows last in both
// directions, never first and never scattered. Every function here reads figures the page
// already holds; none computes one.
var SORT_COLUMNS = {
  instance: function (s) { return (s.display_name || s.shape_code || "").toLowerCase(); },
  provider: function (s) { return (s.provider_key || "").toLowerCase(); },
  card: function (s) { return s.gpu_name ? s.gpu_name.toLowerCase() : null; },
  vram: function (s) { return s.whole_machine_vram_gb; },
  cost: function (s) {
    return s.on_demand_price_per_hour === null || s.on_demand_price_per_hour === undefined
      ? null : s.on_demand_price_per_hour;
  },
  availability: function (s) {
    return state.availabilityKnown ? s.orderable_locations.length : null;
  },
  // No value at all when no model is chosen yet, so those rows sort last in either
  // direction rather than being read as "does not fit".
  models: function (s) {
    return s.model_fit ? (s.model_fit.verdict === "launchable" ? 1 : 0) : null;
  }
};

function sortRows(list) {
  var column = state.sort.column;
  if (!column) return list; // unsorted: the order the answer arrived in, per row._arrivalIndex
  var get = SORT_COLUMNS[column];
  var direction = state.sort.direction;
  var arr = list.slice();
  arr.sort(function (a, b) {
    var av = get(a), bv = get(b);
    var aNull = av === null || av === undefined;
    var bNull = bv === null || bv === undefined;
    if (aNull && bNull) return a._arrivalIndex - b._arrivalIndex;
    if (aNull) return 1;
    if (bNull) return -1;
    var cmp = typeof av === "string" ? av.localeCompare(bv) : av - bv;
    if (direction === "desc") cmp = -cmp;
    return cmp === 0 ? a._arrivalIndex - b._arrivalIndex : cmp;
  });
  return arr;
}

function updateSortIndicators() {
  $$(".sort-th").forEach(function (btn) {
    var th = btn.closest("th");
    if (btn.dataset.col !== state.sort.column || !state.sort.direction) {
      th.setAttribute("aria-sort", "none");
      $(".sort-ind", btn).textContent = "";
    } else {
      th.setAttribute("aria-sort", state.sort.direction === "asc" ? "ascending" : "descending");
      $(".sort-ind", btn).textContent = state.sort.direction === "asc" ? "^" : "v";
    }
  });
  var mobileSelect = byId("mobile-sort-select");
  var value = state.sort.column ? state.sort.column + "-" + state.sort.direction : "";
  if (mobileSelect.value !== value) mobileSelect.value = value;
}

// Below the wide breakpoint the table header row, and the sort buttons inside it, are visually
// hidden, so this dropdown is the only way to reach sorting by mouse or touch. It sets exactly
// the same `state.sort` the header buttons set and calls the same render.
var MOBILE_SORT_OPTIONS = [
  { value: "", label: "Arrival order (unsorted)" },
  { value: "cost-asc", label: "$/hr: low to high" },
  { value: "cost-desc", label: "$/hr: high to low" },
  { value: "vram-asc", label: "VRAM, all cards: low to high" },
  { value: "vram-desc", label: "VRAM, all cards: high to low" },
  { value: "instance-asc", label: "Machine: A to Z" },
  { value: "instance-desc", label: "Machine: Z to A" },
  { value: "provider-asc", label: "Provider: A to Z" },
  { value: "provider-desc", label: "Provider: Z to A" },
  { value: "card-asc", label: "Card: A to Z" },
  { value: "card-desc", label: "Card: Z to A" },
  { value: "availability-asc", label: "Availability: fewest regions first" },
  { value: "availability-desc", label: "Availability: most regions first" },
  { value: "models-asc", label: "Chosen model: does not fit first" },
  { value: "models-desc", label: "Chosen model: fits first" }
];
function buildMobileSort() {
  var select = byId("mobile-sort-select");
  select.innerHTML = MOBILE_SORT_OPTIONS.map(function (opt) {
    return '<option value="' + opt.value + '">' + escapeHtml(opt.label) + "</option>";
  }).join("");
  select.value = state.sort.column + "-" + state.sort.direction;
  select.addEventListener("change", function () {
    if (!select.value) {
      state.sort.column = null;
      state.sort.direction = null;
    } else {
      var parts = select.value.split("-");
      state.sort.column = parts[0];
      state.sort.direction = parts[1];
    }
    updateSortIndicators();
    if (state.loaded) renderTable();
  });
}

$$(".sort-th").forEach(function (btn) {
  btn.addEventListener("click", function () {
    var col = btn.dataset.col;
    if (state.sort.column === col) {
      state.sort.direction = state.sort.direction === "asc" ? "desc"
        : state.sort.direction === "desc" ? null : "asc";
      if (!state.sort.direction) state.sort.column = null;
    } else {
      state.sort.column = col;
      state.sort.direction = "asc";
    }
    updateSortIndicators();
    if (state.loaded) renderTable();
  });
});

// Whether one row survives the filters and the search box. Every one of these reads a figure
// already on the row; none of them asks a provider anything, and none of them touches the
// Allowed / All switch, applied separately.
function passesExtraFilters(s) {
  if (state.filters.provider && s.provider_key !== state.filters.provider) return false;
  if (state.filters.availableOnly &&
    !(state.availabilityKnown && s.orderable_locations.length > 0)) return false;
  if (state.filters.hasModelOnly &&
    !(s.model_fit && s.model_fit.verdict === "launchable")) return false;
  if (state.search) {
    // Both names go in: display_name is the short one shown in the table ("RTX PRO 4000 x1")
    // and shape_code is the provider's full one ("NVIDIA RTX PRO 4000 Blackwell x1"), so a word
    // from either one matches.
    var haystack = [
      s.display_name || "",
      s.shape_code || "",
      s.gpu_name || "",
      s.provider_key || "",
      (s.orderable_locations || []).join(" ")
    ].join(" ").toLowerCase();
    if (haystack.indexOf(state.search) === -1) return false;
  }
  return true;
}

function populateProviderFilter() {
  var sel = byId("filter-provider");
  var current = sel.value;
  var providers = [];
  state.shapes.forEach(function (s) {
    if (s.provider_key && providers.indexOf(s.provider_key) === -1) providers.push(s.provider_key);
  });
  providers.sort();
  sel.innerHTML = '<option value="">All providers</option>' + providers.map(function (p) {
    return '<option value="' + escapeHtml(p) + '">' + escapeHtml(p) + "</option>";
  }).join("");
  sel.value = providers.indexOf(current) !== -1 ? current : "";
  state.filters.provider = sel.value;
}

byId("search-box").addEventListener("input", function (ev) {
  state.search = ev.target.value.toLowerCase();
  if (state.loaded) renderTable();
});
byId("filter-provider").addEventListener("change", function (ev) {
  state.filters.provider = ev.target.value;
  if (state.loaded) renderTable();
});
byId("filter-available").addEventListener("change", function (ev) {
  state.filters.availableOnly = ev.target.checked;
  if (state.loaded) renderTable();
});
byId("filter-has-model").addEventListener("change", function (ev) {
  state.filters.hasModelOnly = ev.target.checked;
  if (state.loaded) renderTable();
});
byId("filtbtn").addEventListener("click", function () {
  var open = !byId("filters").classList.contains("open");
  byId("filters").classList.toggle("open", open);
  this.setAttribute("aria-expanded", String(open));
});
$$('input[name="show-mode"]').forEach(function (r) {
  r.addEventListener("change", function () {
    state.showAll = $('input[name="show-mode"]:checked').value === "all";
    if (state.loaded) renderTable();
  });
});

// ---------------------------------------------------------------------------------------------
// The table.
// ---------------------------------------------------------------------------------------------
var TABLE_COLUMNS = 9;

function availCell(locations, availabilityKnown) {
  if (!availabilityKnown) {
    return '<span class="chip wait"><span class="dot"></span>could not find out</span>';
  }
  if (locations.length > 0) {
    return '<span class="chip ok"><span class="dot"></span>Orderable: ' +
      escapeHtml(locations.join(", ")) + "</span>";
  }
  return '<span class="chip bad"><span class="dot"></span>Not orderable now</span>';
}

// One row's answer for the ONE model chosen in the dropdown above the table: whether this
// machine can serve it, and what is left over when it can, or why not in a few words when it
// cannot. `fit` is `null` before any model is chosen.
function fitCell(fit) {
  if (state.stale) {
    return '<span class="fit-no">' + (state.recalcFailed ? "out of date" : "recalculating\u2026") + "</span>";
  }
  if (!fit) return '<span class="fit-no">choose a model above</span>';
  // The model's name is not repeated here: it is in the dropdown above the table.
  if (fit.verdict === "launchable") {
    return '<span class="fit-yes">' + escapeHtml(fit.reason) + "</span>";
  }
  return '<span class="fit-no">' + escapeHtml(fit.reason) + "</span>";
}

// The one short line under the card name in the Card cell: architecture and year, the PROVIDER's
// own per-card memory (never the vendor's), and whether this card serves FP8. A fact this row's
// server answer did not carry reads as "unknown", never left blank.
function cardFactsLine(s) {
  var archYear = s.card_architecture
    ? s.card_architecture + " " + (s.card_year !== null && s.card_year !== undefined ? s.card_year : "unknown")
    : "unknown";
  var gb = (s.card_per_gpu_vram_gb !== null && s.card_per_gpu_vram_gb !== undefined)
    ? s.card_per_gpu_vram_gb + " GB per card"
    : "unknown GB per card";
  var fp8 = s.card_fp8 === true ? "yes" : s.card_fp8 === false ? "no" : "unknown";
  return archYear + ", " + gb + ", FP8 " + fp8;
}

// The one plain sentence for why this row cannot launch, or "" when it can. A row offers Launch
// only when this has nothing to say; otherwise its Why not button, and a click on the row, show
// this sentence with no Launch button, so a click is never silent.
function rowCannotLaunchReason(s) {
  if (state.stale && state.recalcFailed) {
    return "The model column is out of date because its last update failed. Change a figure to try again.";
  }
  if (state.stale) return "The model column is recalculating for the figures just changed. Try again in a moment.";
  if (!s.is_ticked) return "This machine is not allowed, so it may not be rented. Tick its Allowed box to rent it.";
  if (!s.model_fit) return "No model is chosen. Choose a model above first.";
  if (s.model_fit.verdict !== "launchable") {
    return s.model_fit.reason || "This machine cannot serve the chosen model at these figures.";
  }
  if (!state.availabilityKnown) return "Availability could not be read, so no location is known to supply this machine.";
  if (!(s.orderable_locations && s.orderable_locations.length > 0)) {
    return "No location can supply this machine right now.";
  }
  return "";
}

function renderTable() {
  var body = byId("instance-body");
  closeLaunchRow(); // the row it was anchored to no longer exists once the body is rebuilt
  var baseline = state.shapes.filter(function (s) {
    return state.showAll || s.is_ticked;
  });
  var list = sortRows(baseline.filter(passesExtraFilters));
  var activeFilters = (state.filters.provider ? 1 : 0) + (state.filters.availableOnly ? 1 : 0) +
    (state.filters.hasModelOnly ? 1 : 0);
  byId("filtn").textContent = activeFilters ? " (" + activeFilters + ")" : "";
  byId("visible-count").textContent =
    "Showing " + list.length + " of " + baseline.length +
    (baseline.length === 1 ? " machine" : " machines");
  if (list.length === 0) {
    var reason;
    if (state.shapes.length === 0) {
      reason = state.loadFailed
        ? "The table did not load. Press Refresh to try again."
        : "No machine is on offer at the enabled providers.";
    } else if (baseline.length === 0) {
      reason = state.showAll
        ? "No machine is on offer at the enabled providers."
        : "No machine is allowed. Tick a row, or choose All.";
    } else {
      reason = "No machine matches the filters and search. Clear one to see rows again.";
    }
    body.innerHTML = '<tr class="empty-row"><td colspan="' + TABLE_COLUMNS + '" class="empty">' +
      escapeHtml(reason) + "</td></tr>";
    return;
  }
  body.innerHTML = list.map(function (s, i) {
    var launchable = rowCannotLaunchReason(s) === "";
    var priceCell = s.on_demand_price_per_hour === null
      ? "price unknown"
      : dollars(s.on_demand_price_per_hour);
    var perHourLabel = s.on_demand_price_per_hour === null ? "" : '<span class="cell-label">/hr</span>';
    var card = escapeHtml((s.gpu_name || "unknown card") + " x" + s.gpu_count);
    var name = escapeHtml(s.display_name || s.shape_code);
    var actionBtn = launchable
      ? '<button class="btn sm primary rent" type="button" aria-expanded="false" aria-label="Rent ' + name + '">Rent&hellip;</button>'
      : '<button class="btn sm rent" type="button" aria-expanded="false" aria-label="Why ' + name + ' cannot be rented">Why not?</button>';
    // The <span class="cell-label"> in a cell only shows below the wide breakpoint, where the
    // table header row is visually hidden. Above it the column's own <th> already says this, so
    // the label stays display:none there and nothing is said twice. Each row's tick box has its
    // own id and name, numbered by its place in this render.
    return '<tr data-shape-code="' + escapeHtml(s.shape_code) + '" data-provider-key="' +
      escapeHtml(s.provider_key) + '"' + (launchable ? "" : ' class="dim"') + ">" +
      '<td class="c-allow"><label class="check"><input type="checkbox" id="allow-' + i + '" name="allow_' + i + '" autocomplete="off" data-code="' + escapeHtml(s.shape_code) + '"' +
      (s.is_ticked ? " checked" : "") + ' aria-label="Allowed to rent: ' + escapeHtml(s.shape_code) +
      '"><span class="al">Allowed</span></label></td>' +
      '<td class="c-name inst mono">' + name + "</td>" +
      '<td class="c-prov"><span class="cell-label">Provider: </span>' + escapeHtml(s.provider_key) + "</td>" +
      '<td class="c-card mono">' + card + '<span class="facts">' + escapeHtml(cardFactsLine(s)) + "</span></td>" +
      '<td class="c-vram mono"><span class="cell-label">VRAM, all cards: </span>' + s.whole_machine_vram_gb + " GB</td>" +
      '<td class="c-cost mono">' + escapeHtml(priceCell) + perHourLabel + "</td>" +
      '<td class="c-avail">' + availCell(s.orderable_locations, state.availabilityKnown) + "</td>" +
      '<td class="c-fit">' + fitCell(s.model_fit) + "</td>" +
      '<td class="act">' + actionBtn + "</td></tr>";
  }).join("");
}

async function onCheckboxChanged(cb) {
  var code = cb.dataset.code;
  var checked = cb.checked;
  cb.disabled = true;
  try {
    var response = await fetch(
      "/api/shapes/" + encodeURIComponent(code) + "/permission?checked=" + checked,
      { method: "POST" }
    );
    if (!response.ok) {
      var body = await response.json().catch(function () { return {}; });
      cb.checked = !checked;
      showMsg("Could not change whether '" + code + "' is allowed: " + (body.detail || response.status));
    } else {
      var shape = state.shapes.find(function (s) { return s.shape_code === code; });
      if (shape) shape.is_ticked = checked;
      if (!state.showAll) renderTable();
    }
  } catch (e) {
    cb.checked = !checked;
    showMsg("Could not reach the server to change whether '" + code + "' is allowed.");
  } finally {
    cb.disabled = false;
  }
}

// ---------------------------------------------------------------------------------------------
// The chosen model: dropdown, context, chats, batching boxes and "Model settings".
// ---------------------------------------------------------------------------------------------

// populateModelSelect fills the dropdown with every model the catalog answer named, once,
// keeping whatever was already chosen. Each option's text is the model's name and its format in
// brackets, because different formats need different serving software and a provider may not be
// able to serve one at all, and the table's fit column says which. A hybrid model gets a note
// after the format in the same brackets, because its sum is slightly optimistic.
function populateModelSelect(models) {
  var sel = byId("model-select");
  var current = sel.value;
  var names = models.map(function (m) { return m.name; });
  sel.innerHTML = '<option value="">Choose a model</option>' + models.map(function (m) {
    var label = m.name + " [" + m.format +
      (m.is_hybrid ? ", hybrid state, sum is optimistic" : "") + "]";
    return '<option value="' + escapeHtml(m.name) + '">' + escapeHtml(label) + "</option>";
  }).join("");
  sel.value = names.indexOf(current) !== -1 ? current : "";
  syncBatchingBoxes();
}

// The model-settings boxes: label, box and an "i" note each, mapped to GET/PUT
// /api/models/<name>/settings' own key names. Context and chats keep their boxes in "What to
// run" and are never in this list. "thinking" is the one switch: it always carries a determinate
// value, so it is read with `.checked`, never `.value`.
var MODEL_SETTING_BOXES = [
  { key: "temperature", id: "ms-temperature", label: "Temperature",
    about: "Higher is more varied, lower more predictable." },
  { key: "top_k", id: "ms-top-k", label: "Top K",
    about: "Only the K likeliest next tokens are considered." },
  { key: "top_p", id: "ms-top-p", label: "Top P",
    about: "Only the likeliest tokens that add up to this share are considered." },
  { key: "min_p", id: "ms-min-p", label: "Min P",
    about: "Drops tokens far less likely than the best one." },
  { key: "repeat_penalty", id: "ms-repeat-penalty", label: "Repeat penalty",
    about: "Above 1 discourages the model repeating itself." },
  { key: "gpu_memory_utilization", id: "ms-gpu-mem-util", label: "GPU memory use",
    about: "Share of card memory the engine may claim. vLLM only." },
  { key: "thinking", id: "ms-thinking", label: "Thinking", checkbox: true,
    about: "On unless a saved value of 0 turns it off. Sent with every launch." }
];

function buildModelSettingBoxes() {
  byId("msgrid").innerHTML = MODEL_SETTING_BOXES.map(function (box) {
    var info = infoButton(box.id + "-note", box.label);
    var note = '<p class="note" id="' + box.id + '-note" hidden></p>';
    if (box.checkbox) {
      return '<div class="field"><div class="lab"><span id="' + box.id + '-lab">' + escapeHtml(box.label) + "</span>" + info +
        '</div><label class="switch"><input type="checkbox" role="switch" id="' + box.id + '" name="' + box.key +
        '" autocomplete="off" checked aria-labelledby="' + box.id + '-lab"><span id="' + box.id + '-state">On</span></label>' + note + "</div>";
    }
    return '<div class="field"><div class="lab"><label for="' + box.id + '">' + escapeHtml(box.label) + "</label>" + info +
      '</div><input type="number" step="any" id="' + box.id + '" name="' + box.key + '" autocomplete="off" placeholder="not set">' + note + "</div>";
  }).join("");
  byId("ms-thinking").addEventListener("change", function () {
    byId("ms-thinking-state").textContent = this.checked ? "On" : "Off";
  });
  // One "i" beside "Model settings" opens or closes every explanation in the block.
  byId("expall").addEventListener("click", function () {
    var on = this.getAttribute("aria-expanded") !== "true";
    this.setAttribute("aria-expanded", String(on));
    $$("#msgrid .info").forEach(function (b) {
      b.setAttribute("aria-expanded", String(on));
      byId(b.getAttribute("aria-controls")).hidden = !on;
    });
  });
  // Open on a wide screen, folded on a phone; a person's own choice is never overridden after.
  byId("ms").open = wide.matches;
}

// numOrEmpty turns a JSON number back into what a box shows, or "" for null/undefined.
function numOrEmpty(v) {
  return v === null || v === undefined ? "" : String(v);
}

// settingLine builds the fixed "model: X . engine: Y" text under one box, from the GET answer's
// own field. field.model is the model's own published value, or null when the publisher does not
// state one, which reads "not published" rather than being left blank. withTokensUnit appends " tokens"
// to a published figure, for the context box alone, where field.model is the model's own
// raw-token maximum rather than the box's own thousands unit.
function settingLine(field, withTokensUnit) {
  var modelText = "not published";
  if (field.model !== null && field.model !== undefined) {
    modelText = String(field.model) + (withTokensUnit ? " tokens" : "");
  }
  return "<b>model:</b> " + escapeHtml(modelText) + " &middot; <b>engine:</b> " + escapeHtml(field.engine_default || "");
}

// The notes under Context and Chats: their fixed explanation, then the chosen model's own line.
var ctxModelLine = "";
var chatsModelLine = "";
function paintFieldNotes() {
  var thousands = parseInt(byId("ctx").value, 10);
  var tokens = isNaN(thousands) ? "" : (thousands * 1024).toLocaleString("en-US");
  byId("ctx-note").innerHTML = "In thousands of tokens: <b>" + escapeHtml(byId("ctx").value || "0") + "</b>" +
    (tokens ? " means " + tokens : "") + ". A launch above the model's own maximum is refused." +
    (ctxModelLine ? "<br>" + ctxModelLine : "");
  byId("chats-note").innerHTML = "Simultaneous chats: how many requests the machine must hold open at once. " +
    "More chats need more card memory for the cache." + (chatsModelLine ? "<br>" + chatsModelLine : "");
}

// loadModelSettings fills the "Model settings" block for name, or hides it when no model is
// chosen. Each box holds only the SAVED value, empty otherwise (the program never picks a value
// for anyone), with its note naming the model's published value and the engine's default. A box
// whose key does not apply to this model's engine (field.applies false) is hidden entirely.
async function loadModelSettings(name) {
  var form = byId("model-settings-form");
  if (!name) {
    form.hidden = true;
    ctxModelLine = "";
    chatsModelLine = "";
    paintFieldNotes();
    return;
  }
  var response;
  try {
    response = await fetch("/api/models/" + encodeURIComponent(name) + "/settings");
  } catch (e) {
    form.hidden = true;
    showMsg("Could not reach the server to read the settings of '" + name + "'.");
    return;
  }
  if (!response.ok) {
    form.hidden = true;
    var refusal = await response.json().catch(function () { return {}; });
    showMsg("Could not read the settings of '" + name + "': " + (refusal.detail || response.status));
    return;
  }
  var data = await response.json();
  if (byId("model-select").value !== name) return; // another model was chosen while this was in flight
  // Context and chats keep what they hold unless the new model carries a saved figure, so these
  // two boxes' VALUES change only then; the note beneath each is always refreshed, since it names
  // the model and engine defaults for whichever model is now chosen.
  ["context_thousands", "chats"].forEach(function (key) {
    var field = (data.settings && data.settings[key]) || {};
    var boxId = key === "context_thousands" ? "ctx" : "chats";
    if (field.saved !== null && field.saved !== undefined) {
      byId(boxId).value = numOrEmpty(field.saved);
    }
    if (key === "context_thousands") ctxModelLine = settingLine(field, true);
    else chatsModelLine = settingLine(field, false);
  });
  paintFieldNotes();
  syncBatchingBoxes();
  MODEL_SETTING_BOXES.forEach(function (box) {
    var field = (data.settings && data.settings[box.key]) || {};
    var input = byId(box.id);
    var fieldDiv = input.closest(".field");
    if (!field.applies) {
      fieldDiv.hidden = true;
      return;
    }
    fieldDiv.hidden = false;
    if (box.checkbox) {
      // Checked when the saved value is 1 or absent, unchecked when 0.
      input.checked = field.saved === null || field.saved === undefined || field.saved === 1;
      byId("ms-thinking-state").textContent = input.checked ? "On" : "Off";
    } else {
      input.value = numOrEmpty(field.saved);
    }
    byId(box.id + "-note").innerHTML = escapeHtml(box.about) + "<br>" + settingLine(field, false);
  });
  form.hidden = false;
}

byId("model-settings-form").addEventListener("submit", async function (ev) {
  ev.preventDefault();
  var name = byId("model-select").value;
  if (!name) return;
  var saveBtn = byId("model-settings-save");
  // It PUTs every box: the ones in this form, plus the Context and Chats boxes in "What to run",
  // which this form does not repeat. "Save" writes 1 or 0 for the thinking switch, never empty.
  var body = {
    context_thousands: byId("ctx").value,
    chats: byId("chats").value
  };
  MODEL_SETTING_BOXES.forEach(function (box) {
    var input = byId(box.id);
    body[box.key] = box.checkbox ? (input.checked ? "1" : "0") : input.value;
  });
  saveBtn.disabled = true;
  try {
    var response = await fetch("/api/models/" + encodeURIComponent(name) + "/settings", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body)
    });
    var data = await response.json().catch(function () { return {}; });
    if (!response.ok) {
      showMsg(data.detail || "Could not save the settings.");
    } else {
      await loadModelSettings(name);
    }
  } catch (e) {
    showMsg("Could not reach the server to save the settings.");
  } finally {
    saveBtn.disabled = false;
  }
});

// currentModelIsGGUF answers whether the model chosen in the dropdown right now serves through
// llama.cpp (format "gguf") rather than vLLM. It decides which batching boxes "What to run" shows.
function currentModelIsGGUF() {
  var name = byId("model-select").value;
  var m = state.models.find(function (x) { return x.name === name; });
  return !!m && m.format === "gguf";
}

// deriveBatchedTokens mirrors VLLMServeArgs's own arithmetic (internal/provider/vllm.go) so the
// batched-tokens box can show what an empty box means without a round trip: T = min(8192,
// contextTokens), B = max(3*T, concurrentChats). ctxThousands is the page's own unit (one unit =
// 1024 tokens).
function deriveBatchedTokens(ctxThousands, chats) {
  var contextTokens = ctxThousands * 1024;
  var prefillThreshold = Math.min(8192, contextTokens);
  var batched = 3 * prefillThreshold;
  if (chats > batched) batched = chats;
  return batched;
}

// llama.cpp's own compiled-in defaults for the three per-launch boxes a GGUF row gets instead of
// the batched-tokens box. [DOC] `tools/server/README.md:52-53,223` at tag b10818: `-b,
// --batch-size N` default 2048, `-ub, --ubatch-size N` default 512, `--cache-reuse N` default 0
// (off).
var LLAMA_CPP_BATCH_BOXES = [
  { id: "batch-b", param: "batch_tokens", label: "Batch (-b)", note: "Engine default: 2048." },
  { id: "batch-ub", param: "micro_batch_tokens", label: "Ubatch (-ub)", note: "Engine default: 512." },
  { id: "batch-cache-reuse", param: "cache_reuse_tokens", label: "Cache reuse", note: "The --cache-reuse chunk size. Engine default: 0 (off)." }
];
function buildBatchBoxes() {
  byId("batch-gguf").innerHTML = LLAMA_CPP_BATCH_BOXES.map(function (box) {
    return '<div class="field"><div class="lab"><label for="' + box.id + '">' + escapeHtml(box.label) +
      "</label>" + infoButton(box.id + "-note", box.label) + "</div>" +
      '<input type="number" id="' + box.id + '" name="' + box.param + '" autocomplete="off" min="0" inputmode="numeric">' +
      '<p class="note" id="' + box.id + '-note" hidden>' + escapeHtml(box.note) +
      " Sent with each launch and never saved. Empty means the engine default.</p></div>";
  }).join("");
}

// syncBatchingBoxes shows the batching boxes that belong to the chosen model's engine (the
// batched-tokens box for vLLM, the llama.cpp boxes for GGUF), never both, and keeps the
// batched-tokens placeholder at the figure an empty box would send.
function syncBatchingBoxes() {
  var gguf = currentModelIsGGUF();
  byId("batch-vllm").hidden = gguf;
  byId("batch-gguf").hidden = !gguf;
  var ctxThousands = parseInt(byId("ctx").value, 10) || 0;
  var chats = parseInt(byId("chats").value, 10) || 0;
  byId("batched-tokens").placeholder = ctxThousands && chats
    ? "derived: " + deriveBatchedTokens(ctxThousands, chats) : "derived";
}

// ---------------------------------------------------------------------------------------------
// Loading the table, and recalculating the model column by itself.
// ---------------------------------------------------------------------------------------------
async function loadTable() {
  var ctx = byId("ctx").value;
  var chats = byId("chats").value;
  var model = byId("model-select").value;
  var response;
  try {
    response = await fetch(
      "/api/machines?context=" + encodeURIComponent(ctx) +
      "&chats=" + encodeURIComponent(chats) + "&model=" + encodeURIComponent(model)
    );
  } catch (e) {
    state.loadFailed = true;
    showMsg("Could not reach the server to read the machine table.");
    renderTable();
    return;
  }
  if (!response.ok) {
    var refusal = await response.json().catch(function () { return {}; });
    state.loadFailed = true;
    showMsg(refusal.detail || "The console could not read the machine table.");
    renderTable();
    return;
  }
  var data = await response.json();
  state.loadFailed = false;
  // `_arrivalIndex` records the order the answer arrived in, per row, so a third click on a
  // sorted column heading (state.sort.column === null) has an order to return to.
  data.shapes.forEach(function (s, i) { s._arrivalIndex = i; });
  state.shapes = data.shapes;
  state.availabilityKnown = data.availability_known;
  state.stale = false;
  state.recalcFailed = false;
  (data.availability_problems || []).forEach(function (problem) { showMsg(problem, "warn"); });
  state.models = data.models || [];
  populateModelSelect(state.models);
  populateProviderFilter();
  renderTable();
  // The boxes changed while this answer was in flight: its verdicts are for the old figures.
  if (byId("ctx").value !== ctx || byId("chats").value !== chats || byId("model-select").value !== model) {
    markStaleAndRecalculate(0);
  }
}

// The one path that loads the table: the Refresh button runs it, and so does page open.
async function refreshMachineTable() {
  state.loaded = true;
  byId("table-overlay").hidden = false;
  byId("refresh-btn").disabled = true;
  try {
    await loadTable();
  } finally {
    byId("table-overlay").hidden = true;
    byId("refresh-btn").disabled = false;
  }
}
byId("refresh-btn").addEventListener("click", refreshMachineTable);

// Any change to the model, context or chats recalculates the model column by itself. changeSeq
// numbers the changes, so an answer that arrives after a newer change is discarded and the column
// always matches the boxes as they are now. The number boxes wait for typing to pause.
var changeSeq = 0;
var recalcTimer = null;
function markStaleAndRecalculate(delayMs) {
  changeSeq++;
  if (!state.stale || state.recalcFailed) {
    state.stale = true;
    state.recalcFailed = false;
    if (state.loaded) renderTable();
  }
  clearTimeout(recalcTimer);
  if (!state.loaded) return;
  if (delayMs > 0) recalcTimer = setTimeout(recalculateModels, delayMs);
  else recalculateModels();
}
["ctx", "chats"].forEach(function (id) {
  byId(id).addEventListener("input", function () {
    paintFieldNotes();
    syncBatchingBoxes();
    markStaleAndRecalculate(400);
  });
});
byId("model-select").addEventListener("change", function () {
  syncBatchingBoxes();
  markStaleAndRecalculate(0);
  loadModelSettings(this.value);
});

// recalculateModels asks nothing of any provider. It recomputes the model verdicts alone, at the
// figures now in the boxes, against the machines already on the page: their prices and
// availability came from the last Refresh and are left untouched here. A refusal is shown in the
// message overlay and the column reads "out of date" until a figure changes again.
async function recalculateModels() {
  var mySeq = changeSeq;
  var ctx = byId("ctx").value;
  var chats = byId("chats").value;
  var model = byId("model-select").value;
  var response;
  try {
    response = await fetch(
      "/api/models?context=" + encodeURIComponent(ctx) +
      "&chats=" + encodeURIComponent(chats) + "&model=" + encodeURIComponent(model)
    );
  } catch (e) {
    if (mySeq !== changeSeq) return;
    state.recalcFailed = true;
    showMsg("Could not reach the server to update the model column. It shows the last figures, which may be out of date.");
    renderTable();
    return;
  }
  if (mySeq !== changeSeq) return; // a newer change is pending and will recalculate itself
  if (!response.ok) {
    var refusal = await response.json().catch(function () { return {}; });
    if (mySeq !== changeSeq) return;
    state.recalcFailed = true;
    showMsg((refusal.detail || "The console could not update the model column.") +
      " The column shows the last figures, which may be out of date.");
    renderTable();
    return;
  }
  var data = await response.json();
  if (mySeq !== changeSeq) return;
  var byCode = {};
  data.shapes.forEach(function (entry) { byCode[entry.shape_code] = entry.model_fit; });
  state.shapes.forEach(function (shape) {
    shape.model_fit = byCode[shape.shape_code] || null;
  });
  state.stale = false;
  state.recalcFailed = false;
  renderTable();
}

// ---------------------------------------------------------------------------------------------
// The launch panel, under the row it was opened from.
// ---------------------------------------------------------------------------------------------
var openLaunchRow = null; // the <tr class="panel-row"> currently shown, or null
var pendingLaunch = null; // {shape, vals, offer, built} while the offer is shown in place of the panel

function closeLaunchRow() {
  if (openLaunchRow && openLaunchRow.parentNode) openLaunchRow.parentNode.removeChild(openLaunchRow);
  openLaunchRow = null;
  pendingLaunch = null;
  $$(".rent[aria-expanded=true]").forEach(function (b) { b.setAttribute("aria-expanded", "false"); });
}

// rememberedProject is the project this browser's last launch used, if it still exists.
function rememberedProject() {
  var saved = null;
  try { saved = localStorage.getItem(PROJECT_KEY); } catch (e) { saved = null; }
  if (saved && state.projects.some(function (p) { return p.slug === saved; })) return saved;
  return DEFAULT_PROJECT;
}
function rememberProject(slug) {
  try { localStorage.setItem(PROJECT_KEY, slug); } catch (e) {}
}

// whereLabel turns a "where" select value into the phrase the confirmation sentence names the
// choice with: "" for "any", since nothing is named yet and the provider picks. "any-eu" is
// provider.IsEULocation, which counts the EU, Iceland and Norway.
function whereLabel(where) {
  switch (where) {
    case "any-eu": return "in the EU, Iceland or Norway";
    case "any-us": return "in the US";
    case "any": return "";
    default: return "in " + where;
  }
}

// Every price on the page is written by these two: dollars and cents, and the hourly form.
function dollars(n) { return "$" + n.toFixed(2); }
function priceText(n) { return n === null ? "price unknown" : dollars(n) + "/hr"; }

// launchSentence is the panel's own confirmation sentence, naming the machine, its price, the
// project it is launched for, what it will serve, and the where choice. It is rebuilt on the
// "where" or "Project" select's own change, never only once at open, so the sentence never falls
// out of step with what is actually chosen.
function launchSentence(s, servingText, where, project) {
  var name = s.display_name || s.shape_code;
  var place = whereLabel(where);
  return 'Rent <span class="mono">' + escapeHtml(name) + "</span> (" + escapeHtml(s.gpu_name || "") + ") on " +
    escapeHtml(s.provider_key) + (place ? " " + place : "") +
    ' at <span class="mono">' + escapeHtml(priceText(s.on_demand_price_per_hour)) + "</span> for project '" + escapeHtml(project) + "'. " + servingText;
}

// launchServingText names what will be served. A model row whose own trust_remote_code is set
// (state.models, from the catalog) adds a second sentence: that row's launch runs the
// repository's own Python on the machine, per --trust-remote-code.
function launchServingText() {
  var model = byId("model-select").value;
  if (!model) return "";
  var text = "It will be asked to serve " + escapeHtml(model) + ".";
  var m = state.models.find(function (x) { return x.name === model; });
  if (m && m.trust_remote_code) {
    text += " The machine will run " + escapeHtml(model) + "'s own repository code to do it.";
  }
  return text;
}

function infoButton(controls, label) {
  return '<button class="info" type="button" aria-expanded="false" aria-controls="' + controls +
    '" aria-label="About ' + escapeHtml(label) + '"><span aria-hidden="true">i</span></button>';
}

// launchPanelHTML builds the panel. vals, when given, restores the three boxes a person had set
// before the offer replaced the panel.
function launchPanelHTML(s, vals) {
  var projectChoice = vals ? vals.project : rememberedProject();
  var projectOptions = state.projects.map(function (p) {
    return '<option value="' + escapeHtml(p.slug) + '"' + (p.slug === projectChoice ? " selected" : "") + ">" +
      escapeHtml(p.name) + (p.auto_move ? " (auto-move on)" : "") + "</option>";
  }).join("");
  // "Any", "Any in the EU, Iceland or Norway" and "Any in the US" first, then every location the
  // machine's own live availability named, in the same order the availability chip lists them.
  // Default Any.
  var whereChoice = vals ? vals.where : "any";
  var whereValues = ["any", "any-eu", "any-us"].concat(s.orderable_locations || []);
  var whereLabels = { "any": "Any", "any-eu": "Any in the EU, Iceland or Norway", "any-us": "Any in the US" };
  var whereOptions = whereValues.map(function (w) {
    return '<option value="' + escapeHtml(w) + '"' + (w === whereChoice ? " selected" : "") + ">" +
      escapeHtml(whereLabels[w] || w) + "</option>";
  }).join("");
  var idle = vals ? vals.idle : "30";
  var price = s.on_demand_price_per_hour === null ? "" : " for " + priceText(s.on_demand_price_per_hour);
  return '<div class="lp">' +
    '<p class="sentence">' + launchSentence(s, launchServingText(), whereChoice, projectChoice) + "</p>" +
    '<div class="frow grid">' +
    '<div class="field"><label class="lab" for="lp-project">Project</label><select id="lp-project" name="launch_project" autocomplete="off" class="lp-project">' + projectOptions + "</select></div>" +
    '<div class="field"><label class="lab" for="lp-where">Where</label><select id="lp-where" name="launch_where" autocomplete="off" class="lp-where">' + whereOptions + "</select></div>" +
    '<div class="field"><div class="lab"><label for="lp-idle">Destroy after idle (min)</label>' + infoButton("lp-idle-note", "Destroy after idle") + "</div>" +
    '<input type="number" id="lp-idle" name="launch_idle_minutes" autocomplete="off" class="lp-idle" min="0" value="' + escapeHtml(idle) + '" inputmode="numeric">' +
    '<p class="note" id="lp-idle-note" hidden>Destroyed after this many minutes with no request through GPUGate. <b>0</b> means never. You can change it later on the machine\'s own card.</p></div>' +
    "</div>" +
    '<div class="actions"><button class="btn lp-cancel" type="button">Cancel</button>' +
    '<button class="btn primary lp-go" type="button">Launch' + price + "</button></div></div>";
}

function openLaunchRowFor(tr, s) {
  var sameShape = !!openLaunchRow &&
    openLaunchRow.dataset.shapeCode === s.shape_code &&
    openLaunchRow.dataset.providerKey === s.provider_key;
  closeLaunchRow();
  if (sameShape) return; // the same row clicked again just puts it away
  var row = document.createElement("tr");
  row.className = "panel-row";
  row.dataset.shapeCode = s.shape_code;
  row.dataset.providerKey = s.provider_key;
  var td = document.createElement("td");
  td.colSpan = TABLE_COLUMNS;
  var why = rowCannotLaunchReason(s);
  td.innerHTML = why === ""
    ? launchPanelHTML(s, null)
    : '<div class="whybox" role="status"><p>' + escapeHtml(why) + '</p><button class="x lp-cancel" type="button" aria-label="Close this message">' + X_ICON + "</button></div>";
  row.appendChild(td);
  tr.parentNode.insertBefore(row, tr.nextSibling);
  openLaunchRow = row;
  var rent = $(".rent", tr);
  if (rent) rent.setAttribute("aria-expanded", "true");
  row.scrollIntoView({ block: "nearest", behavior: "smooth" });
}

function shapeOfRow(row) {
  return state.shapes.find(function (x) {
    return x.shape_code === row.dataset.shapeCode && x.provider_key === row.dataset.providerKey;
  });
}

// readPanel reads the three boxes the panel itself owns.
function readPanel(panel) {
  var idle = $(".lp-idle", panel).value;
  return {
    project: $(".lp-project", panel).value,
    where: $(".lp-where", panel).value,
    idle: idle !== "" ? idle : "30"
  };
}

// launchParams builds the POST /api/launch query for the machine named by providerKey and
// shapeCode: the panel's own boxes come in as vals, every other setting is read from "What to
// run" unchanged.
function launchParams(vals, providerKey, shapeCode) {
  var params =
    "idle_minutes=" + encodeURIComponent(vals.idle) +
    "&provider=" + encodeURIComponent(providerKey) +
    "&shape_code=" + encodeURIComponent(shapeCode) +
    "&model=" + encodeURIComponent(byId("model-select").value) +
    "&context=" + encodeURIComponent(byId("ctx").value) +
    "&chats=" + encodeURIComponent(byId("chats").value) +
    "&where=" + encodeURIComponent(vals.where) +
    "&project=" + encodeURIComponent(vals.project);
  // The sampling boxes and gpu_memory_utilization, read from the "Model settings" block. An
  // empty box sends no flag, so a blank value is left out of the query string entirely rather
  // than sent as "". The thinking switch always carries a determinate value, so it is always
  // sent, as "1" or "0".
  MODEL_SETTING_BOXES.forEach(function (box) {
    var input = byId(box.id);
    if (box.checkbox) {
      params += "&" + box.key + "=" + (input.checked ? "1" : "0");
      return;
    }
    if (input.value !== "") params += "&" + box.key + "=" + encodeURIComponent(input.value);
  });
  // The batching boxes in "What to run", never saved, sent only when they carry a figure: the
  // batched-tokens box for a vLLM row, or the llama.cpp boxes for a GGUF row, never both.
  if (currentModelIsGGUF()) {
    LLAMA_CPP_BATCH_BOXES.forEach(function (box) {
      var value = byId(box.id).value;
      if (value !== "") params += "&" + box.param + "=" + encodeURIComponent(value);
    });
  } else if (byId("batched-tokens").value !== "") {
    params += "&max_num_batched_tokens=" + encodeURIComponent(byId("batched-tokens").value);
  }
  return params;
}

// sendLaunch posts the launch and, once the server accepted it, closes the panel. A refusal is
// shown in the message overlay and puts the buttons back; nothing else is launched.
async function sendLaunch(vals, providerKey, shapeCode, buttons) {
  buttons.forEach(function (b) { b.disabled = true; });
  try {
    var response = await fetch("/api/launch?" + launchParams(vals, providerKey, shapeCode), { method: "POST" });
    var body = await response.json().catch(function () { return {}; });
    if (!response.ok) {
      showMsg(body.detail || ("Could not launch this machine: " + response.status));
      buttons.forEach(function (b) { b.disabled = false; });
      return false;
    }
    rememberProject(vals.project); // this browser's next launch panel opens on this project
    // The panel's job was only the confirmation, so it closes once the request is accepted. Then
    // the page goes to the strip so a person sees the machine start, and asks the strip to
    // refresh now rather than at its next poll.
    closeLaunchRow();
    htmx.trigger(byId("rlist"), "load");
    window.scrollTo({ top: 0, behavior: "smooth" });
    byId("run-h").focus({ preventScroll: true });
    return true;
  } catch (e) {
    showMsg("Could not reach the server to launch this machine.");
    buttons.forEach(function (b) { b.disabled = false; });
    return false;
  }
}

// fetchLaunchOffer asks the server whether a cheaper machine (auto-move on) or a machine of the
// same card with fewer cards can serve this launch. It rents nothing. Anything that stops it
// answering reads as "no offer": the launch pressed then goes ahead as it always did.
async function fetchLaunchOffer(params) {
  try {
    var response = await fetch("/api/launch-offer?" + params);
    if (!response.ok) return null;
    var body = await response.json();
    return body.offer ? body : null;
  } catch (e) {
    return null;
  }
}

// offerHTML is the panel body when Launch found a better machine. offer.cheaper (auto-move on, a
// cheaper machine can serve this) and offer.fewer (the chosen machine has more cards than this
// launch needs) name a machine each; one machine that is both is listed once. The last choice
// launches the chosen machine anyway, and turns auto-move off for the project first only when
// the cheaper offer applied. Nothing is rented until a launch button is pressed.
function offerHTML(offer, project) {
  var x = offer.chosen;
  var cheaper = offer.cheaper || null, fewer = offer.fewer || null;
  var same = !!(cheaper && fewer && cheaper.provider_key === fewer.provider_key && cheaper.shape_code === fewer.shape_code);
  var alternatives = [];
  if (cheaper) alternatives.push({ m: cheaper, why: same ? "cheaper and has fewer cards" : "cheaper" });
  if (fewer && !same) alternatives.push({ m: fewer, why: "fewer cards" });
  var title = alternatives.length > 1 ? "Better machines can serve this"
    : same ? "A cheaper machine with fewer cards can serve this"
    : cheaper ? "A cheaper machine can serve this"
    : "This machine has more cards than this launch needs";
  var intro = [];
  if (cheaper) {
    intro.push("Auto-move is on for <b>" + escapeHtml(project) + "</b>, and it would move you to a cheaper machine anyway.");
  }
  if (fewer) {
    intro.push("The chosen machine has " + x.gpu_count + " cards; the launch fits on fewer, and a multi-card machine is where a load can stall.");
  }
  var html = '<div class="lp offer" role="group" aria-labelledby="offer-h"><h3 id="offer-h" tabindex="-1">' + title + "</h3>" +
    "<p>" + intro.join(" ") + " Same model, context and chats. Nothing is rented until you choose.</p>";
  alternatives.forEach(function (alt, i) {
    var m = alt.m;
    var diff = x.price_per_hour - m.price_per_hour;
    var delta = diff > 0
      ? '<span class="save">Saves ' + priceText(diff) + ".</span>"
      : '<span class="d">' + priceText(-diff) + " more.</span>";
    html += '<button class="opt' + (i === 0 ? " best" : "") + '" type="button" data-alt="' + i + '">' +
      '<span class="t">Launch ' + escapeHtml(m.display_name || m.shape_code) + " on " + escapeHtml(m.provider_name) +
      " at " + escapeHtml(priceText(m.price_per_hour)) + "</span>" +
      '<span class="d">' + delta + " " + m.gpu_count + (m.gpu_count === 1 ? " card" : " cards") + ", " + alt.why + ".</span></button>";
  });
  html += '<button class="opt opt-chosen" type="button"><span class="t">Launch ' + escapeHtml(x.display_name || x.shape_code) +
    " anyway at " + escapeHtml(priceText(x.price_per_hour)) + "</span>" +
    (cheaper ? '<span class="d warn">Turns auto-move off for ' + escapeHtml(project) + " first.</span>" : "") + "</button>";
  html += '<div class="actions"><button class="btn lp-back" type="button">Cancel</button></div></div>';
  return { html: html, alternatives: alternatives, cheaper: cheaper };
}

async function doLaunch(btn) {
  var panel = btn.closest(".lp");
  var row = btn.closest(".panel-row");
  var s = shapeOfRow(row);
  if (!s) return;
  var vals = readPanel(panel);
  var label = btn.textContent;
  var buttons = $$("button", panel);
  buttons.forEach(function (b) { b.disabled = true; });
  btn.textContent = "Launching\u2026";
  var offer = await fetchLaunchOffer(launchParams(vals, row.dataset.providerKey, row.dataset.shapeCode));
  if (offer) {
    var built = offerHTML(offer, vals.project);
    pendingLaunch = { shape: s, vals: vals, offer: offer, built: built };
    $("td", row).innerHTML = built.html;
    byId("offer-h").focus();
    return;
  }
  var accepted = await sendLaunch(vals, row.dataset.providerKey, row.dataset.shapeCode, buttons);
  if (!accepted) btn.textContent = label;
}

// A launch button of the offer: one of the alternatives, or the chosen machine anyway.
async function onOfferPick(opt) {
  var row = opt.closest(".panel-row");
  var p = pendingLaunch;
  if (!p) return;
  var buttons = $$("button", row);
  if (opt.classList.contains("opt-chosen")) {
    var x = p.offer.chosen;
    if (p.built.cheaper) {
      buttons.forEach(function (b) { b.disabled = true; });
      try {
        var response = await fetch("/api/projects/" + encodeURIComponent(p.vals.project) + "/auto-move?on=false", { method: "POST" });
        if (!response.ok) {
          var body = await response.json().catch(function () { return {}; });
          showMsg((body.detail || "Could not turn auto-move off.") + " Nothing was launched.");
          buttons.forEach(function (b) { b.disabled = false; });
          return;
        }
      } catch (e) {
        showMsg("Could not reach the server to turn auto-move off. Nothing was launched.");
        buttons.forEach(function (b) { b.disabled = false; });
        return;
      }
      loadProjects();
    }
    sendLaunch(p.vals, x.provider_key, x.shape_code, buttons);
    return;
  }
  var alt = p.built.alternatives[parseInt(opt.dataset.alt, 10)].m;
  sendLaunch(p.vals, alt.provider_key, alt.shape_code, buttons);
}

byId("instance-body").addEventListener("click", function (ev) {
  var goBtn = ev.target.closest(".lp-go");
  if (goBtn) { doLaunch(goBtn); return; }
  var cancel = ev.target.closest(".lp-cancel");
  if (cancel) {
    var anchor = openLaunchRow && openLaunchRow.previousElementSibling;
    closeLaunchRow();
    var rent = anchor && $(".rent", anchor);
    if (rent) rent.focus();
    return;
  }
  var back = ev.target.closest(".lp-back");
  if (back && openLaunchRow && pendingLaunch) {
    var s = pendingLaunch.shape;
    var vals = pendingLaunch.vals;
    pendingLaunch = null;
    $("td", openLaunchRow).innerHTML = launchPanelHTML(s, vals);
    byId("lp-project").focus();
    return;
  }
  var opt = ev.target.closest(".opt");
  if (opt) { onOfferPick(opt); return; }
  if (ev.target.closest(".panel-row")) return; // any other click inside the panel does nothing
  if (ev.target.closest(".c-allow")) return;   // the tick box keeps its own meaning
  var tr = ev.target.closest("tr");
  if (!tr || !tr.dataset.shapeCode) return;
  var shape = shapeOfRow(tr);
  if (!shape) { closeLaunchRow(); return; }
  openLaunchRowFor(tr, shape);
});

byId("instance-body").addEventListener("change", function (ev) {
  var cb = ev.target.closest(".c-allow input");
  if (cb) { onCheckboxChanged(cb); return; }
  // The confirmation sentence names the current choice, so it is rebuilt whenever the panel's
  // own "where" or "Project" select changes, never left reading the default it opened with.
  var select = ev.target.closest(".lp-where, .lp-project");
  if (!select) return;
  var row = select.closest(".panel-row");
  var s = shapeOfRow(row);
  var p = $(".sentence", row);
  if (!s || !p) return;
  p.innerHTML = launchSentence(s, launchServingText(), $(".lp-where", row).value, $(".lp-project", row).value);
});

// ---------------------------------------------------------------------------------------------
// The running strip: server-rendered HTML that htmx swaps into #rlist every five seconds.
// ---------------------------------------------------------------------------------------------
var rlist = byId("rlist");
var refocusIdle = null; // {providerKey, providerId} of the idle box a person just changed
var lastRunningSignature = null; // what the strip showed after the previous swap

// runningSignature names the machines on the strip and the state each is in. The Projects view
// shows which models are serving, so it is reloaded only when this changes.
function runningSignature() {
  return $$("li.mach", rlist).map(function (li) {
    var del = $(".rdelete", li);
    var who = del ? del.dataset.providerKey + "/" + del.dataset.providerId : $(".name", li).textContent;
    return li.dataset.run + ":" + who;
  }).sort().join("|");
}

// afterRunningSwap reads the fresh strip: the "could not ask" notices move into the message
// overlay, and the status pill and menu count are painted from the machine cards.
function afterRunningSwap() {
  var texts = [];
  $$("li.notice", rlist).forEach(function (li) {
    texts.push(li.textContent);
    li.remove();
  });
  syncPollMessages(texts);

  var count = { serving: 0, loading: 0, starting: 0, unconfirmed: 0, running: 0 };
  $$("li.mach", rlist).forEach(function (li) {
    var kind = li.dataset.run;
    if (count[kind] !== undefined) count[kind]++;
  });
  var total = count.serving + count.loading + count.starting + count.unconfirmed + count.running;
  var pill = byId("statuspill");
  var text, cls = "pill";
  if (count.unconfirmed > 0) { text = "Unconfirmed"; cls += " warn"; }
  else if (total === 0) { text = "Idle"; cls += " pill-idle"; }
  else if (count.starting === total) { text = total + " starting"; cls += " warn"; }
  else { text = total + " running"; if (count.serving + count.running !== total) cls += " warn"; }
  pill.className = cls;
  byId("pilltxt").textContent = text;
  var parts = [];
  if (count.serving) parts.push(count.serving + " serving");
  if (count.loading) parts.push(count.loading + " loading");
  if (count.starting) parts.push(count.starting + " starting up");
  if (count.running) parts.push(count.running + " running");
  if (count.unconfirmed) parts.push(count.unconfirmed + " unconfirmed");
  pill.setAttribute("aria-label", "Machines: " + (parts.length ? parts.join(", ") : "none running"));
  byId("navcount").textContent = total === 0 ? "" : text;

  if (refocusIdle) {
    var box = $('.idleinp[data-provider-key="' + refocusIdle.providerKey + '"][data-provider-id="' + refocusIdle.providerId + '"]', rlist);
    if (box) box.focus();
    refocusIdle = null;
  }

  var signature = runningSignature();
  if (lastRunningSignature !== null && signature !== lastRunningSignature) loadProjects();
  lastRunningSignature = signature;
}
rlist.addEventListener("htmx:afterSwap", afterRunningSwap);

// A swap is held back while an idle box has focus, otherwise the number being typed would be
// wiped.
rlist.addEventListener("htmx:beforeSwap", function (ev) {
  var active = document.activeElement;
  if (active && active.classList && active.classList.contains("idleinp")) {
    ev.detail.shouldSwap = false;
  }
});

// A machine's own idle box on the strip: a change posts at once, on a loading machine too (it
// counts once the machine serves).
rlist.addEventListener("change", async function (ev) {
  var input = ev.target.closest(".idleinp");
  if (!input) return;
  var minutes = parseInt(input.value, 10);
  if (isNaN(minutes) || minutes < 0) {
    showMsg("Could not change the idle limit: it must be 0 or more (0 means never).");
    htmx.trigger(rlist, "load");
    return;
  }
  input.disabled = true;
  try {
    var response = await fetch(
      "/api/machines/" + encodeURIComponent(input.dataset.providerKey) + "/" +
      encodeURIComponent(input.dataset.providerId) + "/idle-minutes?minutes=" + encodeURIComponent(minutes),
      { method: "POST" }
    );
    if (!response.ok) {
      var body = await response.json().catch(function () { return {}; });
      showMsg("Could not set the idle limit: " + (body.detail || response.status));
    }
  } catch (e) {
    showMsg("Could not reach the server to set the idle limit.");
  } finally {
    input.disabled = false;
    refocusIdle = { providerKey: input.dataset.providerKey, providerId: input.dataset.providerId };
    htmx.trigger(rlist, "load");
  }
});

// Delete: the strip re-renders its whole list every 5 seconds, so the Delete buttons inside it
// are never the same DOM nodes twice - one delegated listener on the list itself is what keeps
// working across every re-render.
rlist.addEventListener("click", function (ev) {
  var btn = ev.target.closest(".rdelete");
  if (!btn || btn.disabled) return;
  var name = btn.dataset.machineName || "this machine";
  var providerKey = btn.dataset.providerKey, providerId = btn.dataset.providerId;
  confirmBox({
    t: "Destroy this machine?",
    d: "This destroys " + name + " for good. It cannot be undone, and it stops the billing.",
    tgt: name + (btn.dataset.machineSub ? " \u00b7 " + btn.dataset.machineSub : ""),
    ok: "Destroy machine",
    from: btn,
    run: async function () {
      var response = await fetch(
        "/api/machines/" + encodeURIComponent(providerKey) + "/" + encodeURIComponent(providerId) + "/destroy",
        { method: "POST" }
      );
      var body = await response.json().catch(function () { return {}; });
      if (!response.ok) return "Could not destroy " + name + ": " + (body.detail || response.status);
      htmx.trigger(rlist, "load");
      return null;
    }
  });
});

// ---------------------------------------------------------------------------------------------
// Projects: every project GET /api/projects lists, each with its own fixed address, a copy
// button, an auto-move switch, and a Delete on every one except the default project.
// ---------------------------------------------------------------------------------------------
async function loadProjects() {
  var response;
  try { response = await fetch("/api/projects"); } catch (e) {
    showMsg("Could not reach the server to read the projects.");
    return;
  }
  if (!response.ok) {
    showMsg("Could not read the projects.");
    return;
  }
  var data = await response.json();
  state.projects = data.projects || [];
  renderPriceCap(data.price_cap_per_hour);
  renderProjectsBlock();
}

// The price cap box shows the saved value, empty for no cap. A box the person is typing in is
// left alone: a reload of the projects (see runningSignature) must not overwrite a half-typed
// figure.
function renderPriceCap(cap) {
  var input = byId("price-cap-usd");
  if (document.activeElement === input) return;
  input.value = (cap === null || cap === undefined) ? "" : String(cap);
}

byId("price-cap-form").addEventListener("submit", async function (ev) {
  ev.preventDefault();
  var input = byId("price-cap-usd");
  var button = ev.target.querySelector("button[type=submit]");
  button.disabled = true;
  try {
    var response = await fetch("/api/settings/price-cap?usd=" + encodeURIComponent(input.value.trim()), { method: "POST" });
    var body = await response.json().catch(function () { return {}; });
    if (!response.ok) {
      showMsg(body.detail || "Could not save the price cap.");
    } else {
      input.blur();
      renderPriceCap(body.price_cap_per_hour);
    }
  } catch (e) {
    showMsg("Could not reach the server to save the price cap.");
  } finally {
    button.disabled = false;
  }
});

var AUTO_MOVE_ABOUT = "Off by default. When on, GPUGate looks for a cheaper allowed machine that fits the same launch when you switch it on, when the machine starts serving, and every three minutes. It rents it, moves this address over once it serves, and destroys the old one after a clean minute. It never picks a model or a machine you did not allow.";

function renderProjectsBlock() {
  byId("projcount").textContent = String(state.projects.length);
  byId("project-list").innerHTML = state.projects.map(function (p) {
    var address = location.origin + "/p/" + p.slug + "/v1";
    var serving = (p.models || []).length;
    var deleteBtn = p.slug === DEFAULT_PROJECT ? "" :
      '<button type="button" class="btn danger sm project-delete" data-slug="' + escapeHtml(p.slug) + '">Delete</button>';
    var noteId = "n-am-" + escapeHtml(p.slug);
    return '<li class="card proj"><div class="top"><div><h3>' + escapeHtml(p.name) + '</h3><span class="chip plain">' +
      (serving ? "serving " + serving + (serving === 1 ? " model" : " models") : "no model serving") + "</span></div>" + deleteBtn + "</div>" +
      '<div class="addr"><code>' + escapeHtml(address) + '</code><button class="btn sm copy" type="button" data-a="' + escapeHtml(address) +
      '" aria-label="Copy the address of ' + escapeHtml(p.name) + '">Copy</button></div>' +
      '<div class="field"><div class="lab"><label class="switch"><input type="checkbox" role="switch" id="auto-move-' + escapeHtml(p.slug) + '" name="auto_move_' +
      escapeHtml(p.slug) + '" autocomplete="off" class="project-auto-move-box" data-slug="' +
      escapeHtml(p.slug) + '"' + (p.auto_move ? " checked" : "") + "><span>Auto-move to a cheaper machine</span></label>" +
      infoButton(noteId, "auto-move for " + p.name) + "</div>" +
      '<p class="note" id="' + noteId + '" hidden>' + escapeHtml(AUTO_MOVE_ABOUT) + "</p></div></li>";
  }).join("");
}

// The auto-move switch on every project (off by default): while the project's model serves,
// GPUGate looks for a cheaper allowed machine that can serve the same launch, rents it, and moves
// the project's address onto it. Saved on the server; this only posts the new state.
byId("project-list").addEventListener("change", async function (ev) {
  var box = ev.target.closest(".project-auto-move-box");
  if (!box) return;
  var want = box.checked;
  box.disabled = true;
  try {
    var response = await fetch("/api/projects/" + encodeURIComponent(box.dataset.slug) +
      "/auto-move?on=" + (want ? "true" : "false"), { method: "POST" });
    var body = await response.json().catch(function () { return {}; });
    if (!response.ok) {
      showMsg(body.detail || ("Could not change auto-move for '" + box.dataset.slug + "'."));
      box.checked = !want;
    } else {
      var p = state.projects.find(function (x) { return x.slug === box.dataset.slug; });
      if (p) p.auto_move = want;
    }
  } catch (e) {
    showMsg("Could not reach the server to change auto-move for '" + box.dataset.slug + "'.");
    box.checked = !want;
  } finally {
    box.disabled = false;
  }
});

byId("project-list").addEventListener("click", function (ev) {
  var copy = ev.target.closest(".copy");
  if (copy) {
    try { navigator.clipboard.writeText(copy.dataset.a); } catch (e) {}
    copy.textContent = "\u2713";
    setTimeout(function () { copy.textContent = "Copy"; }, 1500);
    return;
  }
  var btn = ev.target.closest(".project-delete");
  if (!btn || btn.disabled) return;
  var slug = btn.dataset.slug;
  confirmBox({
    t: "Delete project '" + slug + "'?",
    d: "The project and its fixed address are removed. Past sessions stay on file. A project still loading or serving a model is refused.",
    tgt: "/p/" + slug,
    ok: "Delete project",
    from: btn,
    run: async function () {
      var response = await fetch("/api/projects/" + encodeURIComponent(slug), { method: "DELETE" });
      var body = await response.json().catch(function () { return {}; });
      if (!response.ok) return body.detail || ("Could not delete '" + slug + "'.");
      await loadProjects();
      return null;
    }
  });
});

// The "New project" form is htmx: a plain POST with form fields. The server answers a refusal as
// an HTML fragment with a 400; its sentence goes to the message overlay, and nothing is swapped
// into the page either way.
document.body.addEventListener("htmx:beforeSwap", function (ev) {
  if (!ev.detail.target || ev.detail.target.id !== "new-project-result") return;
  ev.detail.shouldSwap = false;
  ev.detail.isError = false;
  if (ev.detail.xhr.status >= 400) {
    var doc = new DOMParser().parseFromString(ev.detail.xhr.responseText || "", "text/html");
    showMsg((doc.body.textContent || "").trim() || "Could not create that project.");
  }
});
byId("new-project-form").addEventListener("htmx:afterRequest", function (ev) {
  if (ev.detail.successful && ev.detail.xhr.status === 200) {
    ev.target.reset();
    loadProjects();
  }
});
byId("new-project-form").addEventListener("htmx:sendError", function () {
  showMsg("Could not reach the server to create that project.");
});

// ---------------------------------------------------------------------------------------------
// Sessions: server-rendered HTML that htmx swaps into #session-body every 30 seconds. Clear list
// asks first, then posts through htmx so the list and its totals are swapped in one round trip.
// ---------------------------------------------------------------------------------------------
byId("sessions-clear-btn").addEventListener("click", function () {
  confirmBox({
    t: "Clear the Sessions list?",
    d: "Past rentals stay on file. This only moves where the list starts reading from.",
    tgt: "Sessions list",
    ok: "Clear list",
    from: this,
    run: function () {
      return htmx.ajax("POST", "/api/settings/sessions-since", { target: "#session-body", swap: "innerHTML" })
        .then(function () { return null; })
        .catch(function () { return "Could not clear the Sessions list."; });
    }
  });
});
byId("session-body").addEventListener("htmx:responseError", function () {
  showMsg("Could not read the sessions.");
});

// ---------------------------------------------------------------------------------------------
// The console log, in a right-hand drawer, newest line first.
// ---------------------------------------------------------------------------------------------

// Every raw line the panel has seen this page load, oldest first, kept so "Show everything"
// can reveal what the curated default view left out without asking the server again.
var allLogLines = [];
var lastLogSequence = null;
var logPrimed = false;

function fmtClock(iso) {
  var d = new Date(iso);
  if (isNaN(d.getTime())) return iso;
  return d.toLocaleTimeString();
}

async function pollLog() {
  var url = "/api/log" + (lastLogSequence !== null ? "?after=" + lastLogSequence : "");
  var response;
  try {
    response = await fetch(url);
  } catch (e) {
    return;
  }
  if (!response.ok) return;
  var data = await response.json();
  if (!data.lines || data.lines.length === 0) { logPrimed = true; return; }
  data.lines.forEach(function (line) {
    lastLogSequence = line.sequence;
    // A line that went badly and arrived after the page opened lights the red dot on the
    // console button, until the drawer is opened.
    if (logPrimed && !logOpen()) {
      var entry = classifyForPanel(line);
      if (entry && entry.outcome === "fail") byId("logbadge").hidden = false;
    }
  });
  logPrimed = true;
  allLogLines = allLogLines.concat(data.lines);
  renderLogPanel();
}

// The subjects a curated panel line may belong to. Each entry decides, from the raw `source` and
// `text` this program already logs, whether a line is that subject at all; a line matching none
// of them never reaches the default panel view. Order matters: the first match wins, so an
// auto-move's own rental lines (source "launch", text starting "Auto-move for") are "an
// auto-move", never "a launch".
var PANEL_SUBJECTS = [
  {
    subject: "an auto-move",
    match: function (line) {
      return line.source === "move" ||
        (line.source === "launch" && line.text.indexOf("Auto-move for ") === 0);
    },
  },
  {
    subject: "a launch",
    match: function (line) { return line.source === "launch"; },
  },
  {
    subject: "a provider answer",
    match: function (line) { return line.source === "providers"; },
  },
  {
    subject: "a destroy",
    match: function (line) {
      return line.source === "instances" &&
        /destroy|confirmed gone|already gone|still billing/i.test(line.text);
    },
  },
  {
    subject: "the proxy",
    match: function (line) { return line.source === "proxy"; },
  },
  {
    subject: "the program",
    match: function (line) {
      return line.source === "server" &&
        (/ is starting$/.test(line.text) || line.text === "the server has stopped");
    },
  },
];

// Words that mark a line as having gone badly or as still being under way. Anything else,
// for a line that already matched one of the subjects above, reads as having gone well. A line
// still under way is tested first, so a loading-status line that quotes its last failed
// model-list attempt ("connection refused") stays "still going"; only a line that settles a
// failure reads "went badly".
var BAD_WORDS = /could not|refused|failed|did not answer|did not give|giving up|gave up|error|not sent/i;
// "still waiting for", "waiting for one more" and "resuming the readiness poll" are fixed
// phrases in the log lines internal/console writes for a launch that has been asked for but not
// yet settled, so such a line is never read as "went well".
var STILL_GOING_WORDS = /is starting$|starting up|still waiting for|waiting for one more|^resuming the readiness poll/i;

function classifyForPanel(line) {
  for (var i = 0; i < PANEL_SUBJECTS.length; i++) {
    if (PANEL_SUBJECTS[i].match(line)) {
      var outcome = "ok";
      if (STILL_GOING_WORDS.test(line.text)) outcome = "going";
      else if (BAD_WORDS.test(line.text)) outcome = "fail";
      return { subject: PANEL_SUBJECTS[i].subject, outcome: outcome, text: line.text };
    }
  }
  return null;
}

var OUTCOME_CHIP = { ok: ["ok", "done"], fail: ["bad", "failed"], going: ["wait", "in progress"] };

function renderLogPanel() {
  var body = byId("log-body");
  var showAll = byId("log-show-all").checked;
  var html = "";
  // Newest first. The default view carries only the fixed subjects, one short sentence and an
  // outcome each; "Show everything" reveals every raw line instead, off by default.
  allLogLines.slice().reverse().forEach(function (line) {
    var entry = classifyForPanel(line);
    if (entry) {
      var chip = OUTCOME_CHIP[entry.outcome];
      html += '<div class="le"><time class="mono">' + escapeHtml(fmtClock(line.at)) + '</time><div><div class="s">' +
        escapeHtml(entry.subject.charAt(0).toUpperCase() + entry.subject.slice(1)) +
        ' <span class="chip ' + chip[0] + '">' + chip[1] + '</span></div><div class="m">' + escapeHtml(entry.text) + "</div></div></div>";
    } else if (showAll) {
      html += '<div class="le"><time class="mono">' + escapeHtml(fmtClock(line.at)) + '</time><div><div class="s">' +
        escapeHtml(line.source) + '</div><div class="m">' + escapeHtml(line.text) + "</div></div></div>";
    }
  });
  body.innerHTML = html;
}

var logToggle = byId("log-toggle");
var logPanel = byId("log-panel");
function logOpen() { return logPanel.classList.contains("open"); }
function setLogOpen(open) {
  logPanel.classList.toggle("open", open);
  byId("log-scrim").hidden = !open;
  logToggle.setAttribute("aria-expanded", String(open));
  [mainEl, navEl, topbarEl].forEach(function (x) { x.inert = open; });
  if (!open) syncNav();
  if (open) {
    byId("logbadge").hidden = true;
    byId("log-close").focus();
  } else {
    logToggle.focus();
  }
}
logToggle.addEventListener("click", function () { setLogOpen(true); });
byId("log-close").addEventListener("click", function () { setLogOpen(false); });
byId("log-scrim").addEventListener("click", function () { setLogOpen(false); });
byId("log-show-all").addEventListener("change", renderLogPanel);

document.addEventListener("keydown", function (ev) {
  if (ev.key !== "Escape") return;
  if (!modalBg.hidden) closeConfirm();
  else if (logOpen()) setLogOpen(false);
  else if (navEl.classList.contains("open")) { setNav(false); byId("ham").focus(); }
});

// init builds the parts of the page that are generated, paints the first state, and starts the
// loads and polls. The running strip and the sessions list start themselves through htmx.
function init() {
  initTheme();
  syncNav();
  route();
  byId("origin").textContent = location.host;
  buildMobileSort();
  buildModelSettingBoxes();
  buildBatchBoxes();
  paintFieldNotes();
  refreshMachineTable();  // the table loads when the page opens, as if Refresh were pressed
  updateSortIndicators();  // the default sort has a caret from the first paint, not the first click
  loadProjects();
  pollLog();
  setInterval(pollLog, 10000);
}
init();
