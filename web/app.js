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

/* foray — the loop, client-side, canned (mirrors `make demo-fake`).
   Question -> proposed rung -> Go -> logit-lens resolves -> finding -> assess ->
   climb -> receipt, with opt-in export. No backend; all state in memory. */

"use strict";

const PROMPT = "The Eiffel Tower is in the city of";
const DISTRACTORS = ["the", "France", "a", "central", "French", "north", "Europe", "the", "a"];

const RUNGS = [
  {
    n: 0, model: "openai-community/gpt2", layers: 12, resolveAt: 9, finalProb: 0.41,
    technique: "logit-lens", engine: "eager",
    hardware: "g7e MIG slice", gpu: "RTX PRO 6000 · 24GB", cost: 0.02,
    why: "cheapest model that could show the effect — cents to find out",
    nnsight: 'with model.trace("The Eiffel Tower is in the city of"):\n    layers = [model.transformer.h[i].output[0].save() for i in range(12)]',
    finding: "Top token sharpens to \u201cParis\u201d by layer 9 (p\u22480.41). The association is present even in a toy model.",
    assess: { decision: "climb", reason: "suggestive in GPT-2 \u2014 confirm it scales to 8B" },
    session: "fake-session-r0"
  },
  {
    n: 1, model: "meta-llama/Llama-3.1-8B", layers: 32, resolveAt: 20, finalProb: 0.78,
    technique: "logit-lens", engine: "eager",
    hardware: "g7 whole card", gpu: "RTX PRO 4500 · 32GB", cost: 0.20,
    why: "confirm the effect scales beyond a toy model",
    nnsight: 'with model.trace("The Eiffel Tower is in the city of"):\n    layers = [model.model.layers[i].output[0].save() for i in range(32)]',
    finding: "\u201cParis\u201d emerges around layer 20 (p\u22480.78) \u2014 stronger and earlier-resolved. The effect scales.",
    assess: { decision: "stop", reason: "answered \u2014 the association holds and strengthens with scale" },
    session: "fake-session-r1"
  }
];

const REDUCED = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
const $ = (s, r = document) => r.querySelector(s);

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

/* ---- synth a per-layer logit-lens curve ---- */
function strata(rung) {
  const out = [];
  for (let L = 0; L < rung.layers; L++) {
    const p = rung.finalProb / (1 + Math.exp(-(L - rung.resolveAt) * 0.8));
    const hit = L >= rung.resolveAt - 2;
    out.push({
      ly: L,
      tok: hit ? "Paris" : DISTRACTORS[L % DISTRACTORS.length],
      prob: Math.max(0.01, p),
      hit
    });
  }
  return out;
}

/* ---- render a strata panel into a container, animate the reveal ---- */
function renderStrata(host, rung, compact) {
  host.innerHTML = "";
  const rows = strata(rung);
  const maxP = Math.max(...rows.map(r => r.prob)); // brightest row anchors the ramp
  rows.forEach((r, idx) => {
    const t = r.prob / maxP; // 0..1 confidence within this panel
    const el = document.createElement("div");
    el.className = "row" + (r.hit ? " hit" : "");
    el.innerHTML =
      `<span class="ly">${r.ly}</span>` +
      `<span class="tok">${r.tok}</span>` +
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

/* ---- header cost meter ---- */
let spent = 0;
function addCost(c) {
  spent += c;
  const v = $("#meter-val");
  v.textContent = "$" + spent.toFixed(2);
  v.classList.add("tick");
  setTimeout(() => v.classList.remove("tick"), 350);
}

/* ---- hero preview: rung 0 resolving on load ---- */
function heroPreview() {
  $("#hero-prompt").innerHTML = "\u201cThe Eiffel Tower is in the city of\u201d \u2192 <b>Paris</b>";
  $("#hero-foot").textContent = "gpt2 \u00b7 12 layers \u00b7 reading the residual stream";
  renderStrata($("#hero-strata"), RUNGS[0], false);
}

/* ---- the loop ---- */
let cursor = 0;

function rungCard(rung) {
  const li = document.createElement("li");
  li.className = "rung";
  li.id = "rung-" + rung.n;
  li.innerHTML = `
    <div class="rung-head">
      <span class="rung-no">rung ${rung.n}</span>
      <span class="rung-model">${rung.model}</span>
      <span class="rung-why">${rung.why}</span>
    </div>
    <dl class="spec">
      <dt>technique</dt><dd>${rung.technique}</dd>
      <dt>engine</dt><dd>${rung.engine}</dd>
      <dt>hardware</dt><dd>${rung.hardware} \u00b7 ${rung.gpu}</dd>
      <dt>cost</dt><dd class="cost">~$${rung.cost.toFixed(2)} / session</dd>
    </dl>
    <details class="nn"><summary>the nnsight it will run</summary><pre>${rung.nnsight.replace(/</g, "&lt;")}</pre></details>
    <div class="rung-actions">
      <button class="go" data-n="${rung.n}">Go</button>
      <span class="go-note" style="font-family:'JetBrains Mono',monospace;font-size:12px;color:var(--faint)">approve this rung \u2014 nothing launches until you do</span>
    </div>
    <div class="run-result" hidden>
      <figure class="lens lens--compact"><div class="strata"></div><div class="lens-foot"></div></figure>
      <p class="finding"></p>
      <div class="assess"></div>
      <div class="export" hidden>
        <span class="lbl">your data</span>
        <button class="btn-ghost dl" data-kind="activations">Download activations</button>
        <button class="btn-ghost dl" data-kind="bundle">Download bundle</button>
        <span class="export-note">Stays in your region by default. Export is a presigned pull from your own bucket \u2014 <code>foray export ${rung.session}</code></span>
      </div>
    </div>`;
  $(".go", li).addEventListener("click", () => runRung(rung, li));
  li.querySelectorAll(".dl").forEach(b =>
    b.addEventListener("click", () => downloadStub(rung, b.dataset.kind, li)));
  return li;
}

function runRung(rung, li) {
  const btn = $(".go", li);
  btn.disabled = true; btn.textContent = "Go \u2713";
  $(".go-note", li).textContent = "running on " + rung.hardware + " \u2026";
  addCost(rung.cost);

  const result = $(".run-result", li);
  result.hidden = false;
  $(".lens-foot", result).textContent = rung.model + " \u00b7 " + rung.layers + " layers \u00b7 resolving \u2026";
  const settle = renderStrata($(".strata", result), rung, true);

  setTimeout(() => {
    $(".go-note", li).textContent = "done \u00b7 session " + rung.session;
    $(".lens-foot", result).textContent = rung.model + " \u00b7 reading the residual stream";
    $(".finding", result).textContent = rung.finding;
    const a = $(".assess", result);
    a.innerHTML = `<span class="verb ${rung.assess.decision}">${rung.assess.decision}</span>` +
                  `<span class="reason">${rung.assess.reason}</span>`;
    $(".export", result).hidden = false;
    li.classList.add("done");

    if (rung.assess.decision === "climb" && RUNGS[rung.n + 1]) {
      cursor = rung.n + 1;
      $("#rungs").appendChild(rungCard(RUNGS[cursor]));
      $("#rung-" + cursor).scrollIntoView({ behavior: REDUCED ? "auto" : "smooth", block: "center" });
    } else {
      showReceipt();
    }
  }, settle);
}

function downloadStub(rung, kind, li) {
  let note = li.querySelector(".export .export-note");
  const url = `https://your-bucket.s3.amazonaws.com/sessions/${rung.session}/${kind}.zip?X-Amz-Signature=\u2026(presigned)`;
  note.innerHTML = `presigned, expires in 15 min \u2014 <code style="color:var(--cyan)">${url}</code>`;
}

function showReceipt() {
  const r = $("#receipt");
  r.hidden = false;
  r.innerHTML = `receipt \u00b7 <b>${cursor + 1}</b> rung(s) run \u00b7 ` +
    `<b>$${spent.toFixed(2)}</b> of $5.00 spent on this question \u00b7 ` +
    `instances self-terminated on idle.`;
  r.scrollIntoView({ behavior: REDUCED ? "auto" : "smooth", block: "center" });
}

/* ---- wire up ---- */
$("#ask").addEventListener("submit", (e) => {
  e.preventDefault();
  const q = $("#q").value.trim() || "why does the model store France as Paris?";
  $("#propose").disabled = true; $("#propose").textContent = "Proposed \u2193";
  const s = $("#session");
  s.hidden = false;
  $("#qline").innerHTML = "\u201c" + q + "\u201d <em>\u2014 cheapest experiment first; climb only if it's worth it.</em>";
  cursor = 0;
  $("#rungs").innerHTML = "";
  $("#rungs").appendChild(rungCard(RUNGS[0]));
  s.scrollIntoView({ behavior: REDUCED ? "auto" : "smooth", block: "start" });
});

heroPreview();
