// Keep the provider and Usage entry points explicit; load each panel only when opened.
window.Sub2API = (() => {
  let editor, generation = 0;
  const active = (host) => host?.isConnected && !host.closest("[hidden]");
  async function mountEditor(host, provider, balance, custom, name) {
    const current = ++generation;
    try {
      editor = await import("./editor.js");
      if (current === generation && host.isConnected) editor.mountEditor(host, provider, balance, custom, name);
    } catch (error) {
      if (current === generation && host.isConnected) host.append(el("p", "s2-error", error.message));
    }
  }
  async function cancelEditor(id) {
    generation++;
    await editor?.cancelEditor(id);
  }
  async function renderUsage(host, refresh = false, opened = false) {
    try {
      const module = await import("./usage.js");
      if (active(host)) await module.renderUsage(host, refresh, opened);
    } catch (error) {
      if (active(host)) host.replaceChildren(el("p", "s2-error", error.message));
    }
  }
  return { mountEditor, cancelEditor, renderUsage };
})();
