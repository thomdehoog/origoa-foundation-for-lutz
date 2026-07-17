// Origoa generic frontend: repository navigation, artifact overview and a
// schema-driven detail view. Deliberately minimal — all structure comes from
// the repository (tree, effective schemas, workflows), not from this client.
import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

interface Meta {
  guid: string; kind: string; type: string; title?: string; hid?: string;
  folder: string; workflows?: Record<string, string>; etag: string;
}
interface SchemaField { id: string; name?: string; type: string; options?: string[] }
interface Schema { fields?: SchemaField[]; workflows?: string[]; displayName?: string }
interface Workflow { initial: string; states: string[]; transitions: { from: string; to: string }[] }

async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, init);
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(body.error ?? res.statusText);
  return body as T;
}

@customElement('origoa-app')
export class OrigoaApp extends LitElement {
  static styles = css`
    :host { display: grid; grid-template-columns: 260px 1fr; height: 100vh; }
    aside { border-right: 1px solid #8884; padding: 12px; overflow: auto; }
    main { display: grid; grid-template-rows: auto 1fr; overflow: hidden; }
    h1 { font-size: 16px; margin: 0 0 8px; }
    input[type=search] { width: 100%; box-sizing: border-box; margin-bottom: 8px; padding: 4px 6px; }
    .folder { cursor: pointer; padding: 2px 4px; border-radius: 4px; white-space: nowrap; }
    .folder:hover, .folder.active { background: #8882; }
    table { width: 100%; border-collapse: collapse; }
    th, td { text-align: left; padding: 4px 8px; border-bottom: 1px solid #8883; }
    tbody tr { cursor: pointer; }
    tbody tr:hover, tbody tr.active { background: #8881; }
    .list { overflow: auto; max-height: 40vh; border-bottom: 2px solid #8884; }
    .detail { overflow: auto; padding: 16px; }
    .field { margin-bottom: 10px; }
    .field label { display: block; font-size: 12px; opacity: .7; }
    .field input, .field select, .field textarea { width: 100%; max-width: 480px; box-sizing: border-box; padding: 4px 6px; }
    .error { color: #c33; }
    .muted { opacity: .6; font-size: 12px; }
    button { padding: 4px 10px; }
    .comments { margin-top: 16px; border-top: 1px solid #8884; padding-top: 8px; max-width: 520px; }
    .comment { border-left: 3px solid #8886; padding-left: 8px; margin: 6px 0; }
  `;

  @state() private artifacts: Meta[] = [];
  @state() private folders: string[] = [];
  @state() private folder = '';
  @state() private selected?: Meta;
  @state() private data?: Record<string, unknown>;
  @state() private schema?: Schema;
  @state() private workflowDefs: Record<string, Workflow> = {};
  @state() private comments: Record<string, unknown>[] = [];
  @state() private etag = '';
  @state() private query = '';
  @state() private error = '';

  connectedCallback() {
    super.connectedCallback();
    this.refresh();
  }

  private async refresh() {
    try {
      const tree = await api<{ folders: string[]; artifacts: Meta[] }>('/api/tree');
      this.folders = tree.folders;
      this.artifacts = tree.artifacts;
      this.error = '';
    } catch (e) { this.error = String(e); }
  }

  private get visible(): Meta[] {
    const q = this.query.toLowerCase();
    return this.artifacts.filter(a =>
      (a.kind === 'entry' || a.kind === 'document') &&
      (this.folder === '' || a.folder === this.folder || a.folder.startsWith(this.folder + '/')) &&
      (q === '' || `${a.title} ${a.hid} ${a.type}`.toLowerCase().includes(q)));
  }

  private async select(m: Meta) {
    try {
      const res = await api<{ meta: Meta; data: Record<string, unknown> }>(`/api/${m.kind === 'document' ? 'documents' : 'entries'}/${m.guid}`);
      this.selected = res.meta;
      this.data = res.data;
      this.etag = res.meta.etag;
      this.schema = undefined;
      this.workflowDefs = {};
      this.comments = (await api<{ comments: Record<string, unknown>[] }>(`/api/artifacts/${m.guid}/comments`)).comments;
      try {
        this.schema = (await api<{ schema: Schema }>(`/api/schemas/effective?type=${encodeURIComponent(m.type)}&path=${encodeURIComponent(m.folder)}`)).schema;
        for (const id of this.schema?.workflows ?? []) {
          this.workflowDefs = { ...this.workflowDefs, [id]: (await api<{ workflow: Workflow }>(`/api/workflows/${id}?path=${encodeURIComponent(m.folder)}`)).workflow };
        }
      } catch { /* untyped artifact: render raw fields */ }
      this.error = '';
    } catch (e) { this.error = String(e); }
  }

  private async save(e: Event) {
    e.preventDefault();
    if (!this.selected) return;
    const form = new FormData(e.target as HTMLFormElement);
    const fields: Record<string, string> = {};
    for (const [k, v] of form.entries()) if (k.startsWith('f:')) fields[k.slice(2)] = String(v);
    try {
      await api(`/api/${this.selected.kind === 'document' ? 'documents' : 'entries'}/${this.selected.guid}`, {
        method: 'PUT',
        headers: { 'If-Match': this.etag, 'Content-Type': 'application/json' },
        body: JSON.stringify({ title: form.get('title'), fields }),
      });
      await this.refresh();
      await this.select(this.selected);
    } catch (err) { this.error = String(err); }
  }

  private async transition(wf: string, to: string) {
    if (!this.selected || !to) return;
    try {
      await api(`/api/artifacts/${this.selected.guid}/transition`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ workflow: wf, to }),
      });
      await this.refresh();
      await this.select(this.selected);
    } catch (err) { this.error = String(err); }
  }

  private async comment(e: Event) {
    e.preventDefault();
    const form = e.target as HTMLFormElement;
    const text = new FormData(form).get('text');
    if (!this.selected || !text) return;
    try {
      await api('/api/comments', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ subject: this.selected.guid, text }),
      });
      form.reset();
      await this.select(this.selected);
    } catch (err) { this.error = String(err); }
  }

  render() {
    return html`
      <aside>
        <h1>Origoa</h1>
        <input type="search" placeholder="Filter…" @input=${(e: Event) => this.query = (e.target as HTMLInputElement).value}>
        <div class="folder ${this.folder === '' ? 'active' : ''}" @click=${() => this.folder = ''}>/ (root)</div>
        ${this.folders.map(f => html`
          <div class="folder ${this.folder === f ? 'active' : ''}" style="padding-left:${8 + 10 * f.split('/').length}px"
               @click=${() => this.folder = f}>${f.split('/').pop()}</div>`)}
      </aside>
      <main>
        <div class="list">
          <table>
            <thead><tr><th>HID</th><th>Title</th><th>Kind</th><th>Type</th><th>State</th></tr></thead>
            <tbody>
              ${this.visible.map(a => html`
                <tr class=${this.selected?.guid === a.guid ? 'active' : ''} @click=${() => this.select(a)}>
                  <td>${a.hid ?? ''}</td><td>${a.title ?? ''}</td><td>${a.kind}</td><td>${a.type}</td>
                  <td>${Object.entries(a.workflows ?? {}).map(([k, v]) => `${k}: ${v}`).join(', ')}</td>
                </tr>`)}
            </tbody>
          </table>
        </div>
        <div class="detail">
          ${this.error ? html`<p class="error">${this.error}</p>` : nothing}
          ${this.selected ? this.renderDetail() : html`<p class="muted">Select an artifact.</p>`}
        </div>
      </main>`;
  }

  private renderDetail() {
    const m = this.selected!;
    const fields = (this.data?.fields ?? {}) as Record<string, unknown>;
    const schemaFields = this.schema?.fields ?? Object.keys(fields).map(id => ({ id, type: 'text' }));
    return html`
      <h1>${m.title || m.guid}</h1>
      <p class="muted">${m.hid ? m.hid + ' · ' : ''}${m.kind} · ${m.type} · /${m.folder} · ${m.guid}</p>
      ${Object.entries(m.workflows ?? {}).map(([wf, state]) => {
        const def = this.workflowDefs[wf];
        const targets = def?.transitions.filter(t => t.from === state).map(t => t.to) ?? [];
        return html`<div class="field"><label>Workflow ${wf}</label>
          <select @change=${(e: Event) => this.transition(wf, (e.target as HTMLSelectElement).value)}>
            <option value="">${state}</option>
            ${targets.map(t => html`<option value=${t}>→ ${t}</option>`)}
          </select></div>`;
      })}
      <form @submit=${this.save}>
        <div class="field"><label>Title</label><input name="title" .value=${m.title ?? ''}></div>
        ${schemaFields.map(f => html`
          <div class="field"><label>${f.name ?? f.id} (${f.type})</label>
            ${f.options?.length
              ? html`<select name="f:${f.id}">${f.options.map(o => html`<option ?selected=${fields[f.id] === o}>${o}</option>`)}</select>`
              : html`<input name="f:${f.id}" .value=${String(fields[f.id] ?? '')}>`}
          </div>`)}
        <button type="submit">Save</button>
      </form>
      <div class="comments">
        <label class="muted">Comments</label>
        ${this.comments.map(c => html`<div class="comment"><span class="muted">${c.author ?? 'anonymous'} · ${c.created}</span><br>${c.text}</div>`)}
        <form @submit=${this.comment}>
          <input name="text" placeholder="Add a comment…">
        </form>
      </div>`;
  }
}
