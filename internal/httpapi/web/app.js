// Native browser module; no build step or runtime dependency.

const element = (name, text) => {
  const node = document.createElement(name);
  if (text !== undefined) node.textContent = String(text);
  return node;
};

const request = async (path, options = {}) => {
  const response = await fetch(path, options);
  const body = response.status === 204 ? null : await response.json().catch(() => null);
  if (!response.ok) throw new Error(body?.error?.message ?? `Request failed (${response.status})`);
  return { body, etag: response.headers.get("etag")?.replaceAll('"', "") ?? "" };
};

class OrigoaApp extends HTMLElement {
  state = { items: [], selected: null, etag: "", schema: {}, links: null, overlay: null, workflows: [], history: [] };

  connectedCallback() {
    this.innerHTML = `
      <div class="shell">
        <header class="topbar">
          <span class="brand">Origoa</span>
          <form class="search" id="search-form" role="search">
            <label class="visually-hidden" for="search">Search repository</label>
            <input id="search" name="q" type="search" maxlength="200" placeholder="Search repository">
            <select name="kind" aria-label="Artifact kind"><option value="">All kinds</option><option>entry</option><option>document</option><option>link</option><option>comment</option></select>
            <input name="type" aria-label="Artifact type" maxlength="64" placeholder="Type">
            <button>Search</button>
          </form>
        </header>
        <aside class="sidebar">
          <div class="actions"><strong>Artifacts</strong><button id="refresh">Refresh</button></div>
          <div id="artifact-list" class="artifact-list"></div>
          <details>
            <summary>Create artifact</summary>
            <form id="create-form">
              <label>Kind<select name="kind"><option>entry</option><option>document</option><option>link</option><option>comment</option></select></label>
              <label>Type<input name="type" value="note" pattern="[A-Za-z][A-Za-z0-9._-]{0,63}" required></label>
              <label>Title<input name="title" maxlength="300" required></label>
              <label>Folder<input name="path" value="artifacts" maxlength="256" required></label>
              <label>Additional properties (JSON object)<textarea name="extra" spellcheck="false">{}</textarea></label>
              <button class="primary">Create</button>
            </form>
          </details>
          <p class="status" id="sidebar-status" role="status"></p>
        </aside>
        <main class="content"><section class="panel" id="detail"><p class="empty">Select an artifact.</p></section></main>
      </div>`;
    this.querySelector("#refresh").addEventListener("click", () => this.load());
    this.querySelector("#search-form").addEventListener("submit", (event) => this.search(event));
    this.querySelector("#create-form").addEventListener("submit", (event) => this.create(event));
    this.load().then(() => {
      const guid = new URL(location.href).searchParams.get("artifact");
      if (guid) this.select(guid);
    });
  }

  async load(path = "/api/artifacts") {
    try {
      const { body } = await request(path);
      this.state.items = body.items;
      this.renderList();
      this.status("");
    } catch (error) { this.status(error.message, true); }
  }

  async search(event) {
    event.preventDefault();
    const data = new FormData(event.currentTarget);
    const parameters = new URLSearchParams();
    for (const name of ["q", "kind", "type"]) {
      const value = String(data.get(name) ?? "").trim();
      if (value) parameters.set(name, value);
    }
    await this.load(`/api/search?${parameters}`);
  }

  renderList() {
    const list = this.querySelector("#artifact-list");
    list.replaceChildren();
    if (!this.state.items.length) list.append(element("p", "No artifacts found."));
    let path = null;
    for (const item of this.state.items) {
      if (item.path !== path) {
        path = item.path;
        list.append(element("h3", path || "Repository root"));
      }
      const button = element("button", item.artifact.title);
      button.className = "artifact";
      button.type = "button";
      button.dataset.guid = item.artifact.guid;
      button.setAttribute("aria-current", String(item.artifact.guid === this.state.selected?.guid));
      button.append(element("small", `${item.artifact.kind} · ${item.artifact.type}`));
      button.addEventListener("click", () => this.select(item.artifact.guid));
      list.append(button);
    }
  }

  async create(event) {
    event.preventDefault();
    const form = event.currentTarget;
    const data = new FormData(form);
    try {
      const extra = JSON.parse(String(data.get("extra")));
      if (!extra || Array.isArray(extra) || typeof extra !== "object") throw new Error("Additional properties must be a JSON object.");
      const payload = { ...extra, kind: data.get("kind"), type: data.get("type"), title: data.get("title"), path: data.get("path") };
      const { body } = await request("/api/artifacts", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(payload) });
      form.reset();
      form.elements.path.value = "artifacts";
      form.elements.type.value = "note";
      form.elements.extra.value = "{}";
      await this.load();
      await this.select(body.artifact.guid);
    } catch (error) { this.status(error.message, true); }
  }

  async select(guid) {
    try {
      const [{ body: item, etag }, schema, links, workflows, history] = await Promise.all([
        request(`/api/artifacts/${guid}`),
        request(`/api/artifacts/${guid}/schema`),
        request(`/api/artifacts/${guid}/links`),
        request(`/api/artifacts/${guid}/workflows`),
        request(`/api/artifacts/${guid}/history`),
      ]);
      this.state.selected = item.artifact;
      this.state.etag = etag;
      this.state.schema = schema.body;
      this.state.links = links.body;
      this.state.workflows = workflows.body.items;
      this.state.history = history.body.items;
      this.state.overlay = item.artifact.base ? (await request(`/api/artifacts/${guid}/overlay`)).body : null;
      window.history.replaceState(null, "", `?artifact=${encodeURIComponent(guid)}`);
      this.renderList();
      this.renderDetail();
      this.status("");
    } catch (error) { this.status(error.message, true); }
  }

  renderDetail() {
    const artifact = this.state.selected;
    const detail = this.querySelector("#detail");
    detail.replaceChildren();
    detail.append(element("h1", artifact.title));
    const meta = element("p", `${artifact.kind} · ${artifact.type} · ${artifact.guid}`);
    meta.className = "meta";
    detail.append(meta);

    const form = element("form");
    const title = this.input("Title", "title", artifact.title);
    const hid = this.input("Human-readable ID", "hid", artifact.hid ?? "");
    form.append(title.label, hid.label);
    const fields = this.renderFields(artifact.fields ?? {});
    form.append(fields.container);
    let content;
    let structuredContent = false;
    if (artifact.kind === "document") {
      structuredContent = artifact.content !== undefined && artifact.content !== null && typeof artifact.content !== "string";
      const value = structuredContent ? JSON.stringify(artifact.content, null, 2) : (artifact.content ?? "");
      content = this.input(structuredContent ? "Document content (JSON)" : "Document content", "content", value, "textarea");
      content.input.className = "document";
      form.append(content.label);
    }
    const actions = element("div");
    actions.className = "actions";
    const save = element("button", "Save");
    save.className = "primary";
    const remove = element("button", "Delete");
    remove.type = "button";
    remove.className = "danger";
    remove.addEventListener("click", () => this.remove());
    actions.append(save, remove);
    form.append(actions);
    form.addEventListener("submit", async (event) => {
      event.preventDefault();
      try {
        const patch = { title: title.input.value, hid: hid.input.value || null, fields: fields.read() };
        if (content) patch.content = structuredContent ? JSON.parse(content.input.value) : content.input.value;
        await this.save(patch);
      } catch (error) { this.status(error.message, true); }
    });
    detail.append(form);

    this.renderWorkflows(detail);
    this.renderLinks(detail);
    if (this.state.overlay) {
      const heading = element("h2", "Resolved overlay");
      const pre = element("pre", JSON.stringify(this.state.overlay, null, 2));
      pre.className = "meta";
      detail.append(heading, pre);
    }
    const heading = element("h2", "History");
    const list = element("ul");
    list.className = "card-list";
    for (const item of this.state.history) list.append(element("li", `${item.subject} · ${new Date(item.date).toLocaleString()}`));
    detail.append(heading, list);
  }

  input(labelText, name, value, kind = "input") {
    const label = element("label", labelText);
    const input = element(kind);
    input.name = name;
    input.value = value;
    label.append(input);
    return { label, input };
  }

  renderFields(values) {
    const container = element("div");
    const definitions = this.state.schema.fields ?? {};
    const inputs = new Map();
    if (!Object.keys(definitions).length) {
      const raw = this.input("Fields (JSON)", "fields", JSON.stringify(values, null, 2), "textarea");
      container.append(raw.label);
      return { container, read: () => JSON.parse(raw.input.value || "{}") };
    }
    for (const [name, definition] of Object.entries(definitions)) {
      const label = element("label", definition.displayName ?? name);
      let input;
      if (definition.type === "boolean") {
        label.className = "checkbox";
        input = element("input");
        input.type = "checkbox";
        input.checked = Boolean(values[name]);
      } else if (definition.type === "enumeration" && Array.isArray(definition.values)) {
        input = element("select");
        for (const value of definition.values) {
          const option = element("option", value);
          option.value = value;
          option.selected = value === values[name];
          input.append(option);
        }
      } else {
        input = element(definition.type === "multi-line" || definition.type === "rich-text" ? "textarea" : "input");
        input.type = ["number", "float", "integer", "currency"].includes(definition.type) ? "number" : "text";
        input.value = values[name] ?? "";
      }
      input.required = Boolean(definition.required);
      label.append(input);
      inputs.set(name, { input, definition });
      container.append(label);
    }
    return { container, read: () => {
      const result = {};
      for (const [name, { input, definition }] of inputs) {
        if (definition.type === "boolean") result[name] = input.checked;
        else if (["number", "float", "integer", "currency"].includes(definition.type)) result[name] = Number(input.value);
        else result[name] = input.value;
      }
      return result;
    } };
  }

  renderWorkflows(detail) {
    if (!this.state.workflows.length) return;
    detail.append(element("h2", "Workflows"));
    for (const workflow of this.state.workflows) {
      const row = element("div");
      row.className = "actions";
      row.append(element("span", `${workflow.id}: ${workflow.state}`));
      for (const transition of workflow.transitions) {
        const button = element("button", transition.label ?? transition.id);
        button.addEventListener("click", () => this.transition(workflow.id, transition.id));
        row.append(button);
      }
      detail.append(row);
    }
  }

  renderLinks(detail) {
    const all = [...this.state.links.incoming, ...this.state.links.outgoing];
    if (!all.length) return;
    detail.append(element("h2", "Relationships"));
    const list = element("ul");
    list.className = "card-list";
    for (const item of all) list.append(element("li", `${item.artifact.linkType}: ${item.artifact.source} → ${item.artifact.target}`));
    detail.append(list);
  }

  async save(patch) {
    try {
      const { body, etag } = await request(`/api/artifacts/${this.state.selected.guid}`, {
        method: "PUT", headers: { "Content-Type": "application/json", "If-Match": `"${this.state.etag}"` }, body: JSON.stringify(patch),
      });
      this.state.etag = etag;
      this.state.selected = body.artifact;
      await this.load();
      await this.select(body.artifact.guid);
    } catch (error) { this.status(error.message, true); }
  }

  async transition(workflow, transition) {
    try {
      const { body } = await request(`/api/artifacts/${this.state.selected.guid}/transitions`, {
        method: "POST", headers: { "Content-Type": "application/json", "If-Match": `"${this.state.etag}"` }, body: JSON.stringify({ workflow, transition }),
      });
      await this.select(body.artifact.guid);
    } catch (error) { this.status(error.message, true); }
  }

  async remove() {
    if (!confirm(`Delete “${this.state.selected.title}”? Git history retains it.`)) return;
    try {
      await request(`/api/artifacts/${this.state.selected.guid}`, { method: "DELETE", headers: { "If-Match": `"${this.state.etag}"` } });
      this.state.selected = null;
      window.history.replaceState(null, "", location.pathname);
      this.querySelector("#detail").replaceChildren(element("p", "Artifact deleted."));
      await this.load();
    } catch (error) { this.status(error.message, true); }
  }

  status(message, error = false) {
    const status = this.querySelector("#sidebar-status");
    status.textContent = message;
    status.className = `status${error ? " error" : ""}`;
  }
}

customElements.define("origoa-app", OrigoaApp);
