"use strict";
// The little glue htmx doesn't cover: filter buttons, clipboard, keyboard
// shortcuts and the live indicator. Everything else is rendered server-side.

const $ = (id) => document.getElementById(id);

// setFilter updates a hidden filter input and re-renders the view.
function setFilter(name, value) {
  const form = $("filters");
  form.querySelector('input[type=hidden][name="' + name + '"]').value = value;
  for (const el of document.querySelectorAll('[data-set="' + name + '"]')) {
    el.classList.toggle(el.classList.contains("db") ? "active" : "on", el.dataset.value === value);
  }
  if (name === "paused") {
    $("pause").textContent = value === "1" ? "Resume" : "Pause";
    $("pause").classList.toggle("hot", value === "1");
  }
  htmx.trigger(form, "change");
}

function setStatus(on, text) {
  if (!$("conn")) return;
  $("conn").className = "dot" + (on ? " on pulse" : "");
  $("connText").textContent = text;
}

function paused() {
  const input = document.querySelector('#filters input[name="paused"]');
  return input && input.value === "1";
}

document.addEventListener("click", (e) => {
  const copy = e.target.closest("[data-copy]");
  if (copy) {
    navigator.clipboard.writeText(copy.dataset.copy).then(() => { copy.textContent = copy.dataset.copied || "Copied ✓"; });
    return;
  }
  if (e.target.closest("#pause")) { setFilter("paused", paused() ? "0" : "1"); return; }
  const set = e.target.closest("[data-set]");
  if (set) setFilter(set.dataset.set, set.dataset.value);
});

document.addEventListener("keydown", (e) => {
  if (!$("q")) return;
  if (e.key === "/" && document.activeElement !== $("q")) { e.preventDefault(); $("q").focus(); }
  if (e.key === " " && document.activeElement === document.body) { e.preventDefault(); $("pause").click(); }
});

document.addEventListener("htmx:sseOpen", () => setStatus(true, "live"));
document.addEventListener("htmx:sseError", () => setStatus(false, "reconnecting…"));
document.addEventListener("htmx:afterSettle", () => { if (paused()) setStatus(false, "paused"); });
document.addEventListener("DOMContentLoaded", () => { if (paused()) setStatus(false, "paused"); });

// Rows stream in at the top: keep the list bounded.
document.addEventListener("htmx:sseMessage", () => {
  const list = $("list");
  while (list && list.children.length > 801) list.lastElementChild.remove();
});
