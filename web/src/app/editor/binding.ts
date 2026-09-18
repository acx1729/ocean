// A two-way binding between one CodeMirror view and one LoroText inside a
// multi-container doc. Editor transactions are written to the text and
// committed with a per-editor origin; every doc event that is not ours
// (remote imports, undo, other local code) is replayed into the view as
// annotated changes so the binding never echoes.

import { Annotation, type ChangeSpec, type Extension } from "@codemirror/state";
import { EditorView, ViewPlugin, type ViewUpdate } from "@codemirror/view";
import type { LoroDoc, LoroEventBatch, LoroText, TextDiff } from "loro-crdt";

export const fromLoro = Annotation.define<boolean>();

export function loroTextBinding(doc: LoroDoc, getText: () => LoroText | null, origin: string): Extension {
  return ViewPlugin.define((view) => {
    const text = getText();
    if (text) {
      const current = text.toString();
      if (view.state.doc.toString() !== current) {
        view.dispatch({
          changes: { from: 0, to: view.state.doc.length, insert: current },
          annotations: fromLoro.of(true),
        });
      }
    }
    const unsubscribe = doc.subscribe((batch: LoroEventBatch) => {
      if (batch.origin === origin) return;
      const t = getText();
      if (!t) return;
      const changes: ChangeSpec[] = [];
      for (const ev of batch.events) {
        if (ev.target !== t.id || ev.diff.type !== "text") continue;
        let pos = 0;
        for (const d of (ev.diff as TextDiff).diff) {
          if (d.insert !== undefined) {
            changes.push({ from: pos, insert: d.insert });
          } else if (d.delete !== undefined) {
            changes.push({ from: pos, to: pos + d.delete });
            pos += d.delete;
          } else if (d.retain !== undefined) {
            pos += d.retain;
          }
        }
      }
      if (changes.length === 0) return;
      // Deltas describe the pre-change document; CodeMirror applies a change set
      // against the same base, so positions can be used as they are.
      try {
        view.dispatch({ changes, annotations: fromLoro.of(true) });
      } catch {
        // Positions diverged (should not happen); resync wholesale.
        view.dispatch({
          changes: { from: 0, to: view.state.doc.length, insert: t.toString() },
          annotations: fromLoro.of(true),
        });
      }
    });
    return {
      update(u: ViewUpdate) {
        if (!u.docChanged) return;
        if (u.transactions.every((tr) => tr.annotation(fromLoro))) return;
        const t = getText();
        if (!t) return;
        // Apply from the end so earlier offsets stay valid.
        const edits: { from: number; to: number; insert: string }[] = [];
        u.changes.iterChanges((fromA, toA, _fromB, _toB, inserted) => {
          edits.push({ from: fromA, to: toA, insert: inserted.toString() });
        });
        for (let i = edits.length - 1; i >= 0; i--) {
          const e = edits[i]!;
          if (e.to > e.from) t.delete(e.from, e.to - e.from);
          if (e.insert.length > 0) t.insert(e.from, e.insert);
        }
        doc.commit({ origin });
      },
      destroy() {
        unsubscribe();
      },
    };
  });
}

export const editorTheme = EditorView.theme({
  "&": { backgroundColor: "transparent" },
  ".cm-content": { caretColor: "var(--fg)" },
  ".cm-cursor": { borderLeftColor: "var(--fg)" },
  "&.cm-focused .cm-selectionBackground, .cm-selectionBackground": { backgroundColor: "var(--accent-2)" },
});
