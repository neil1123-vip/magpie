// A relay can search as Anthropic's or OpenAI's API does; its host can't
// tell magpie so (#359). Native search goes through its Anthropic or
// Responses endpoint: a Chat endpoint has no such tool.
export function mountNativeTest(wrap, control, kind, { provider: p, preset: pr, host: ed }) {
  const form = () => {
    const proxy = proxyOfDraft();
    if (proxy === null) throw new Error(t("Proxy: type its address, like http://127.0.0.1:7890"));
    const own = accountProxiesOfDraft();
    if (own.missing) {
      const line = ed.querySelector(`.acct-proxy[data-key="${own.missing}"]`);
      throw new Error(t("Proxy of {user}: type its address, like http://127.0.0.1:7890", { user: line?.querySelector(".who")?.textContent || own.missing }));
    }
    const body = { ...asTyped(), id: p?.id, picked: (p ? chosenIds() : draft.extra || []).slice(0, DETECT_MAX), authHeader: draft.authHeader || "", proxy, accountProxies: own.map };
    if (!p && pr) {
      for (const k of ["chat", "responses", "anthropic", "gemini"]) {
        if (draft[k] === undefined && !body[k]) body[k] = (pr[k] || "").trim();
      }
    }
    if (!p && draft.copyOf) Object.assign(body, { new: true, copyOf: draft.copyOf });
    return body;
  };
  const words = kind === "search" ? {
    check: "Check search",
    title: "Send native web search requests with the form as typed. Uses the provider's quota; nothing is saved.",
    ok: "Native search confirmed",
    failed: "Search request failed",
    unknown: "Not confirmed: no native search result returned",
    note: "Only the tested model and key are checked. Tick the option yourself to use native search.",
  } : {
    check: "Check compaction",
    title: "Send a native compaction request with the form as typed. Uses the provider's quota; nothing is saved.",
    ok: "Native compaction confirmed",
    failed: "Compaction request failed",
    unknown: "Not confirmed: no encrypted compaction returned",
    note: "Only the tested model and key are checked. Tick the option yourself to use native compaction.",
  };
  const controls = el("div", "detect-row");
  const test = el("button", "text action", t(words.check));
  test.type = "button";
  test.title = t(words.title);
  controls.append(control, test);
  const out = el("div", "native-test-out");
  out.hidden = true;
  out.setAttribute("role", "status");
  out.setAttribute("aria-live", "polite");
  wrap.append(controls, out);
  test.onclick = async () => {
    if (test.disabled) return;
    const xb = extraBodyOf(draft.extraBody);
    if (xb.error) { out.hidden = false; out.replaceChildren(el("div", "hint err", xb.error)); return; }
    test.disabled = true;
    test.classList.add("busy");
    out.hidden = false;
    out.replaceChildren(el("div", "hint", t("Checking…")));
    try {
      const r = await api("provider/test-" + kind, form());
      out.replaceChildren();
      for (const x of r.results) {
        const result = el("div", "hint " + (x.ok ? "ok" : x.error ? "err" : "warn"));
        const label = apiLabel(x.protocol) + (x.model ? " · " + x.model : "");
        result.textContent = label + " · " + (x.ok ? t(words.ok) : x.error ? t(words.failed) + ": " + (x.status ? x.status + " · " : "") + t(x.error) : t(words.unknown));
        out.append(result);
      }
      out.append(el("div", "hint", t(words.note)));
    } catch (e) { out.replaceChildren(el("div", "hint err", e.message)); }
    finally { test.disabled = false; test.classList.remove("busy"); }
  };
}
