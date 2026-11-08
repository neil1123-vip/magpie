// Recovery opt-in belongs to this computer, not the provider's synced draft.
const recoveryDrafts = new WeakMap();

// Native compaction returns the provider's sealed item unchanged (#1099).
// Offered on Responses, ChatGPT and Copilot; show tracks custom URLs.
export function mountCompaction(row, refresh, form, probe, providerID) {
  const [ntk, ncb] = tick(t("Codex compacts on this provider"), !!form.nativeCompaction);
  ntk.classList.add("native-compaction");
  ncb.onchange = () => { form.nativeCompaction = ncb.checked; };
  const hint = el("div");
  hint.append(
    el("div", "", t("When on, Codex's compaction request and encrypted summary pass through unchanged. Recorded summaries stay on their original provider and account unless a usable local recovery copy can restore the history.")),
    el("div", "", t("When off, magpie generates a text summary. The conversation can then continue through magpie across accounts and providers.")),
  );
  const control = el("div", "native-compaction-controls");
  const recovery = el("div", "compaction-recovery");
  recovery.hidden = !form.nativeCompaction;
  control.append(probe ? nativeTestField(ntk, "compaction", probe) : ntk, recovery);
  mountRecovery(recovery, form, ncb, providerID);
  const next = field(t("Compaction"), control, hint);
  row.forEach((e, i) => { e.replaceWith(next[i]); row[i] = next[i]; });
  refresh();
}

export function mountRecovery(wrap, form, nativeBox, providerID) {
  const kept = recoveryDrafts.get(form) || { loaded: false, enabled: false };
  recoveryDrafts.set(form, kept);
  const [label, box] = tick(t("Save local recovery copies"), false);
  label.classList.add("recovery-copy");
  const note = el("div", "hint", t("Off by default. Saves readable history on this computer only, outside backups and sync. Normal compaction makes no extra summary call; restoring history can use more input tokens."));
  const stateLine = el("div", "hint");
  stateLine.setAttribute("role", "status");
  stateLine.setAttribute("aria-live", "polite");
  const tools = el("div", "recovery-tools");
  const capLabel = el("label", "", t("Total storage limit (MiB)"));
  const cap = input("", "", "number");
  cap.classList.add("recovery-limit");
  cap.min = "1";
  cap.step = "1";
  capLabel.append(cap);
  const apply = el("button", "text", t("Apply"));
  const clear = el("button", "text", t("Clear recovery copies"));
  const refresh = el("button", "text", t("Refresh"));
  for (const b of [apply, clear, refresh]) b.type = "button";
  tools.append(capLabel, apply, clear, refresh);
  const warning = el("div", "hint warn");
  const choices = el("div", "recovery-choices");
  const records = el("div", "recovery-records");
  const error = el("div", "hint err");
  error.setAttribute("role", "alert");
  wrap.append(label, note, stateLine, tools, warning, records, choices, error);
  let reading = 0, busy = false;
  const update = () => {
    wrap.hidden = !nativeBox.checked;
    box.checked = form.localRecoveryEnabled ?? kept.enabled;
    box.disabled = !kept.loaded || busy;
    for (const b of [apply, clear, refresh]) b.disabled = busy;
    cap.disabled = busy || !kept.loaded;
    apply.disabled ||= !kept.loaded;
    clear.disabled ||= !kept.loaded;
  };
  box.onchange = () => { form.localRecoveryEnabled = box.checked; };
  nativeBox.addEventListener("change", () => {
    if (!nativeBox.checked) form.localRecoveryEnabled = false;
    update();
  });
  const read = async () => {
    const version = ++reading;
    try {
      const s = await api("compaction-recovery");
      if (version !== reading || !wrap.isConnected) return;
      if (!s.config || !Array.isArray(s.config.providers) || !Number.isSafeInteger(s.config.maxBytes) || s.config.maxBytes <= 0) {
        throw new Error(t("Local recovery settings could not be read."));
      }
      kept.loaded = true;
      kept.enabled = !!providerID && s.config.providers.includes(providerID);
      cap.value = String(Math.ceil(s.config.maxBytes / 1048576));
      stateLine.textContent = t("{used} used, {reserved} reserved, {limit} total; {n} recovery copies", {
        used: fmtBytes(s.bytes || 0), reserved: fmtBytes(s.reservedBytes || 0),
        limit: fmtBytes(s.config.maxBytes), n: (s.records || []).length,
      });
      warning.textContent = (s.bytes || 0) + (s.reservedBytes || 0) >= s.config.maxBytes * 0.9
        ? t("Recovery storage is nearly full. Raise the limit or explicitly clear copies; compaction will fail when a copy cannot be saved.")
        : "";
      records.replaceChildren();
      for (const record of s.records || []) {
        const row = el("div", "recovery-record");
        const description = [record.provider, record.model, fmtBytes(record.bytes || 0), record.lossy ? t("Local excerpt, some history left out") : ""].filter(Boolean).join(" · ");
        row.append(el("span", "hint", description));
        const remove = el("button", "icon", "");
        const title = t("Delete recovery copy {id}", { id: record.id });
        remove.type = "button";
        remove.title = title;
        remove.setAttribute("aria-label", title);
        remove.append(svg("M3 4h10M6 4V2h4v2M4 4l1 10h6l1-10M7 6v6M9 6v6", 14));
        remove.onclick = async () => {
          if (!await confirmAction(title, t("This deletes only the selected recovery copy. A conversation depending on it may no longer recover."), t("Delete"))) return;
          await act(() => api("compaction-recovery/delete", { id: record.id, confirm: true }));
        };
        row.append(remove);
        records.append(row);
      }
      choices.replaceChildren();
      const list = (s.choices || []).filter((c) => c.branch && Array.isArray(c.actions));
      if (list.length) {
        choices.append(el("b", "", t("Requests awaiting a choice")));
        for (const c of list) {
          const row = el("div", "recovery-choice");
          row.append(el("div", "hint", [c.model, c.branch, c.reason].filter(Boolean).join(" · ")));
          if (c.branch === "request") {
            row.append(el("div", "hint", t("This request has no reliable branch identity. Retry with X-Magpie-Recovery-Token: {id} after allowing it.", { id: c.id })));
          }
          for (const action of c.actions) {
            const text = action === "native-only" ? "Allow native compaction once without a copy"
              : action === "excerpt" ? "Allow a local excerpt once" : null;
            if (!text) continue;
            const button = el("button", "text", t(text));
            button.type = "button";
            button.onclick = async () => {
              const consequence = action === "native-only"
                ? "This compaction will have no local recovery copy. This choice applies only to the listed request."
                : "Some history will be left out. This choice applies only to the listed request and branch.";
              if (!await confirmAction(t(text), t(consequence), t("Allow once"))) return;
              await act(() => api("compaction-recovery/allow", { id: c.id, choice: action }));
              if (!error.textContent) status(t("Allowed once. Retry this request in the client."), "ok");
            };
            row.append(button);
          }
          choices.append(row);
        }
      }
      choices.append(el("div", "hint", t("Without a reliable branch identity, full replay is decided for each request; local excerpts cannot be authorized here.")));
      error.textContent = "";
    } catch (e) {
      if (version !== reading || !wrap.isConnected) return;
      error.textContent = e.message;
    } finally { update(); }
  };
  const act = async (action) => {
    if (busy) return;
    busy = true;
    error.textContent = "";
    update();
    try { await action(); await read(); }
    catch (e) { error.textContent = e.message; }
    finally { busy = false; update(); }
  };
  apply.onclick = async () => {
    const n = Number(cap.value);
    const bytes = n * 1048576;
    if (!cap.value || !Number.isSafeInteger(n) || n < 1 || !Number.isSafeInteger(bytes)) {
      error.textContent = t("Give a positive whole number of MiB.");
      cap.focus(NO_SCROLL);
      return;
    }
    await act(() => api("compaction-recovery/configure", { maxBytes: bytes }));
  };
  clear.onclick = async () => {
    if (!await confirmAction(t("Clear recovery copies?"), t("This deletes all local recovery copies. Conversations that need them may no longer recover if their original account is unavailable. Enabled providers keep saving new copies."), t("Clear recovery copies"))) return;
    await act(() => api("compaction-recovery/clear", { confirm: true }));
  };
  refresh.onclick = read;
  update();
  read();
}

// Provider creation must succeed first, so a failed Add leaves no local opt-in.
export async function saveRecovery(form, body) {
  const id = body.id || slug(body.name || body.preset || "");
  const kept = recoveryDrafts.get(form);
  if (form.localRecoveryEnabled === undefined) return;
  const enabled = !!form.nativeCompaction && !!form.localRecoveryEnabled;
  form.localRecoveryEnabled = enabled;
  try {
    await api("compaction-recovery/configure", { provider: id, enabled });
    if (kept) kept.enabled = enabled;
  } catch (e) {
    throw new Error(t("The provider was saved, but its local recovery setting failed: {error}", { error: e.message }));
  }
}
