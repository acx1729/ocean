// Remote carets and selections inside a block editor. The editor screen
// resolves each peer's stable Loro cursors to offsets and pushes them in
// through setRemoteCursors; positions are mapped through local edits between
// updates.

import { type Extension, StateEffect, StateField } from "@codemirror/state";
import { Decoration, type DecorationSet, EditorView, WidgetType } from "@codemirror/view";

export interface RemoteCursor {
  peer: string;
  name: string;
  color: string;
  anchor: number;
  head: number;
}

export const setRemoteCursors = StateEffect.define<RemoteCursor[]>();

class CaretWidget extends WidgetType {
  constructor(
    readonly name: string,
    readonly color: string,
  ) {
    super();
  }
  eq(other: CaretWidget): boolean {
    return other.name === this.name && other.color === this.color;
  }
  toDOM(): HTMLElement {
    const el = document.createElement("span");
    el.className = "remote-caret";
    el.style.setProperty("--caret", this.color);
    el.dataset.name = this.name;
    return el;
  }
  ignoreEvent(): boolean {
    return true;
  }
}

function decorate(cursors: RemoteCursor[], docLength: number): DecorationSet {
  const ranges = [];
  for (const c of cursors) {
    const head = Math.max(0, Math.min(c.head, docLength));
    const anchor = Math.max(0, Math.min(c.anchor, docLength));
    if (anchor !== head) {
      const from = Math.min(anchor, head);
      const to = Math.max(anchor, head);
      ranges.push(
        Decoration.mark({ class: "remote-selection", attributes: { style: `--sel:${c.color}` } }).range(
          from,
          to,
        ),
      );
    }
    ranges.push(Decoration.widget({ widget: new CaretWidget(c.name, c.color), side: 1 }).range(head));
  }
  ranges.sort((a, b) => a.from - b.from || a.value.startSide - b.value.startSide);
  return Decoration.set(ranges, true);
}

const remoteField = StateField.define<{ cursors: RemoteCursor[]; deco: DecorationSet }>({
  create: () => ({ cursors: [], deco: Decoration.none }),
  update(value, tr) {
    let cursors = value.cursors;
    let changed = false;
    for (const e of tr.effects) {
      if (e.is(setRemoteCursors)) {
        cursors = e.value;
        changed = true;
      }
    }
    if (tr.docChanged && !changed) {
      cursors = cursors.map((c) => ({
        ...c,
        anchor: tr.changes.mapPos(c.anchor),
        head: tr.changes.mapPos(c.head),
      }));
      changed = true;
    }
    if (!changed) return value;
    return { cursors, deco: decorate(cursors, tr.state.doc.length) };
  },
  provide: (f) => EditorView.decorations.from(f, (v) => v.deco),
});

export function remotePresence(): Extension {
  return remoteField;
}
