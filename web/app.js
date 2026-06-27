/*
 * Copyright 2026 Scott Friedman
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

/* foray — the page as a thin client over the brain loop (internal/webapi).
   Question -> POST /api/propose -> proposed rung -> Go -> POST /api/approve
   (Cedar-gated launch, trace, interpret, assess) -> finding + recommendation ->
   climb on a fresh Go -> receipt, with opt-in export. The brain proposes and
   interprets; only Go (an /api/approve POST) launches; climbing is never
   automatic. Under `make web-fake` the server runs the offline fake loop, so the
   same page rehearses with no AWS — the rehearsal badge stays honest.

   The page owns only the Go seam and the viz: every number and finding comes from
   the server. The strata panel is an illustrative logit-lens seeded from the
   rung's real layer count; the rendered pixels arrive via vizRef at the deploy
   step (the polished viz is deferred, ARCHITECTURE.md §9). */

"use strict";

const REDUCED = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
const $ = (s, r = document) => r.querySelector(s);

/* ---- API ---- */
async function api(path, body) {
  const resp = await fetch(path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  let data = {};
  try { data = await resp.json(); } catch { /* empty/non-JSON body */ }
  if (!resp.ok) {
    throw new Error(data.error || `request failed (${resp.status})`);
  }
  return data;
}

/* ---- probability colormap (inferno-ish), t in [0,1] ---- */
const STOPS = ["#2A2250", "#7A2E83", "#C03A6E", "#F0852E", "#FBD08A"].map(hexRGB);
function hexRGB(h) { return [1, 3, 5].map(i => parseInt(h.slice(i, i + 2), 16)); }
function ramp(t) {
  t = Math.max(0, Math.min(1, t));
  const x = t * (STOPS.length - 1), i = Math.floor(x), f = x - i;
  const a = STOPS[i], b = STOPS[Math.min(i + 1, STOPS.length - 1)];
  const c = a.map((v, k) => Math.round(v + (b[k] - v) * f));
  return `rgb(${c[0]},${c[1]},${c[2]})`;
}

/* ---- synth an illustrative per-layer confidence curve ---- */
/* Seeded from the rung's real layer count; the shape (a sigmoid resolving about
   two-thirds of the way up) illustrates a logit lens. It is NOT the real result —
   the finding text below it is. The rendered pixels arrive via vizRef at deploy. */
function strata(layers) {
  const n = Math.max(1, layers | 0);
  const resolveAt = Math.round(n * 0.66);
  const finalProb = 0.8;
  const out = [];
  for (let L = 0; L < n; L++) {
    const p = finalProb / (1 + Math.exp(-(L - resolveAt) * 0.8));
    out.push({ ly: L, prob: Math.max(0.01, p), hit: L >= resolveAt - 2 });
  }
  return out;
}

/* ---- render a strata panel into a container, animate the reveal ---- */
function renderStrata(host, layers, compact) {
  host.innerHTML = "";
  const rows = strata(layers);
  const maxP = Math.max(...rows.map(r => r.prob)); // brightest row anchors the ramp
  rows.forEach((r, idx) => {
    const t = r.prob / maxP; // 0..1 confidence within this panel
    const el = document.createElement("div");
    el.className = "row" + (r.hit ? " hit" : "");
    el.setAttribute("aria-hidden", "true"); // decorative; the finding text is the content
    el.innerHTML =
      `<span class="ly">${r.ly}</span>` +
      `<span class="track"><span class="fill"></span></span>` +
      `<span class="pct">${(r.prob * 100).toFixed(0)}%</span>`;
    host.appendChild(el);
    const fill = $(".fill", el);
    const paint = () => {
      el.classList.add("show");
      const col = ramp(t);
      fill.style.background = col;
      fill.style.width = Math.max(4, t * 100).toFixed(1) + "%";
      if (t > 0.62) fill.style.boxShadow = "0 0 12px -2px " + col;
    };
    if (REDUCED) paint();
    else setTimeout(paint, 90 + idx * (compact ? 26 : 55));
  });
  return (REDUCED ? 0 : 90 + rows.length * (compact ? 26 : 55)) + 500;
}

/* ---- header cost meter: bound to the server's numbers, never a client sum ---- */
function setMeter(spentUSD) {
  const v = $("#meter-val");
  v.textContent = "$" + Number(spentUSD || 0).toFixed(2);
  v.classList.add("tick");
  setTimeout(() => v.classList.remove("tick"), 350);
}

/* ---- hero preview: a static, decorative logit-lens illustration on load ---- */
function heroPreview() {
  $("#hero-prompt").innerHTML = "per-layer confidence climbs as the model resolves an answer";
  $("#hero-foot").textContent = "illustrative logit lens · the rendered viz arrives from your own GPU";
  renderStrata($("#hero-strata"), 12, false);
}

/* ---- the loop ---- */
let currentLadder = null; // the carried server state; the client echoes it back

function esc(s) { return String(s).replace(/[&<>]/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;" }[c])); }

function rungCard(rung) {
  const li = document.createElement("li");
  li.className = "rung";
  li.id = "rung-" + rung.index;
  const hw = [rung.hardware, rung.gpu].filter(Boolean).join(" · ") || "—";
  li.innerHTML = `
    <div class="rung-head">
      <span class="rung-no">rung ${rung.index}</span>
      <span class="rung-model">${esc(rung.model)}</span>
      <span class="rung-why">${esc(rung.rationale)}</span>
    </div>
    <dl class="spec">
      <dt>technique</dt><dd>${esc(rung.technique)}</dd>
      <dt>engine</dt><dd>${esc(rung.engine)}</dd>
      <dt>hardware</dt><dd>${esc(hw)}${rung.instance ? " · " + esc(rung.instance) : ""}</dd>
      <dt>cost</dt><dd class="cost">~$${Number(rung.estCostUSD).toFixed(2)} / session</dd>
    </dl>
    <details class="nn"><summary>the nnsight it will run</summary><pre>${esc(rung.nnsight)}</pre></details>
    <div class="rung-actions">
      <button class="go" data-n="${rung.index}">Go</button>
      <span class="go-note">approve this rung — nothing launches until you do</span>
    </div>
    <div class="run-result" hidden>
      <figure class="lens lens--compact" role="img" aria-label="logit-lens illustration">
        <div class="strata"></div>
        <div class="lens-foot"></div>
      </figure>
      <p class="finding" role="status" aria-live="polite"></p>
      <div class="assess"></div>
      <div class="export" hidden>
        <span class="lbl">your data</span>
        <button class="btn-ghost dl" data-kind="activations">Download activations</button>
        <button class="btn-ghost dl" data-kind="bundle">Download bundle</button>
        <span class="export-note"></span>
      </div>
    </div>`;
  $(".go", li).addEventListener("click", () => runRung(rung, li));
  return li;
}

async function runRung(rung, li) {
  const btn = $(".go", li);
  btn.disabled = true;
  btn.setAttribute("aria-busy", "true");
  btn.textContent = "Go ✓";
  const note = $(".go-note", li);
  note.textContent = "launching on " + (rung.hardware || rung.instance || "the chosen tier") + " …";

  const result = $(".run-result", li);
  result.hidden = false;
  const foot = $(".lens-foot", result);
  foot.textContent = rung.model + " · " + rung.layers + " layers · resolving …";
  const settle = renderStrata($(".strata", result), rung.layers, true);

  let resp;
  try {
    resp = await api("/api/approve", { ladder: currentLadder, rungIndex: rung.index });
  } catch (err) {
    btn.removeAttribute("aria-busy");
    note.textContent = "denied: " + err.message; // Cedar/budget denials surface verbatim
    foot.textContent = rung.model + " · not launched";
    li.classList.add("denied");
    return;
  }

  currentLadder = resp.ladder; // advance the carried state
  setMeter(resp.spentUSD);

  // Settle the illustration, then reveal the server's finding + recommendation.
  setTimeout(() => {
    btn.removeAttribute("aria-busy");
    note.textContent = "done · session " + resp.sessionId;
    foot.textContent = rung.model + " · reading the residual stream";
    $(".finding", result).textContent = resp.result.finding;

    const a = $(".assess", result);
    const dec = resp.recommendation.decision;
    a.innerHTML = `<span class="verb ${esc(dec)}">${esc(dec)}</span>` +
      `<span class="reason">${esc(resp.recommendation.reason)}</span>`;

    // Opt-in export of the user's own saves (presigned; never auto-egress).
    const ex = $(".export", result);
    ex.hidden = false;
    $(".export-note", ex).innerHTML =
      `Stays in your region by default. Export is a presigned pull from your own ` +
      `bucket — <code>foray export ${esc(resp.sessionId)}</code>`;
    ex.querySelectorAll(".dl").forEach(b =>
      b.addEventListener("click", () => doExport(resp.sessionId, b.dataset.kind, ex)));

    li.classList.add("done");

    // Climb only on a fresh Go: append the next rung the brain proposed (if any).
    if (dec === "climb" && resp.nextProposal) {
      const next = rungCard(resp.nextProposal);
      $("#rungs").appendChild(next);
      next.scrollIntoView({ behavior: REDUCED ? "auto" : "smooth", block: "center" });
    } else {
      showReceipt(resp);
    }
  }, settle);
}

async function doExport(sessionId, kind, ex) {
  const note = $(".export-note", ex);
  try {
    const link = await api("/api/export", { sessionId, kind });
    note.innerHTML = `presigned (${esc(link.kind)}), expires ${esc(link.expiresAt)} — ` +
      `<code class="url">${esc(link.url)}</code>`;
  } catch (err) {
    note.textContent = "export: " + err.message; // residency/ownership denials, verbatim
  }
}

function showReceipt(resp) {
  const r = $("#receipt");
  r.hidden = false;
  const rungs = (currentLadder && currentLadder.Cursor) || 0;
  r.textContent = `receipt · ${rungs} rung(s) run · ` +
    `$${Number(resp.spentUSD).toFixed(2)} of $${Number(resp.budgetUSD).toFixed(2)} ` +
    `spent on this question · instances self-terminate on idle.`;
  r.scrollIntoView({ behavior: REDUCED ? "auto" : "smooth", block: "center" });
}

/* ---- propose: the brain's first move (a ladder, or a clarifying question) ---- */
async function propose(question) {
  const propose = $("#propose");
  propose.disabled = true;
  propose.setAttribute("aria-busy", "true");
  propose.textContent = "Proposing …";

  const s = $("#session");
  const rungs = $("#rungs");
  const receipt = $("#receipt");
  rungs.innerHTML = "";
  receipt.hidden = true;
  currentLadder = null;

  let resp;
  try {
    resp = await api("/api/propose", { question });
  } catch (err) {
    s.hidden = false;
    $("#qline").textContent = "couldn't reach the brain: " + err.message;
    resetPropose();
    return;
  }

  s.hidden = false;

  // A clarifying question short-circuits: naming a model is the wrong first move.
  if (resp.clarify) {
    $("#qline").innerHTML = "<em>foray needs to know first:</em> " + esc(resp.clarify);
    resetPropose();
    return;
  }

  currentLadder = resp.ladder;
  setMeter(0);
  $("#qline").innerHTML = "“" + esc(question) +
    "” <em>— cheapest experiment first; climb only if it's worth it.</em>";
  rungs.appendChild(rungCard(resp.proposal));
  s.scrollIntoView({ behavior: REDUCED ? "auto" : "smooth", block: "start" });
  resetPropose();
}

function resetPropose() {
  const propose = $("#propose");
  propose.disabled = false;
  propose.removeAttribute("aria-busy");
  propose.textContent = "Propose an experiment";
}

/* ---- wire up ---- */
$("#ask").addEventListener("submit", (e) => {
  e.preventDefault();
  const q = $("#q").value.trim();
  if (!q) { $("#q").focus(); return; }
  propose(q);
});

heroPreview();
