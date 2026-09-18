import { defaultKeymap, insertNewlineAndIndent } from "@codemirror/commands";
import { markdown, markdownLanguage } from "@codemirror/lang-markdown";
import { defaultHighlightStyle, syntaxHighlighting } from "@codemirror/language";
import { EditorSelection, EditorState, Prec } from "@codemirror/state";
import { EditorView, keymap, placeholder as cmPlaceholder } from "@codemirror/view";
import type { LoroDoc, TreeID, UndoManager } from "loro-crdt";
import { useEffect, useLayoutEffect, useRef } from "react";
import { contentText } from "../../lib/blocks";
import { editorTheme, loroTextBinding } from "./binding";
import { type RemoteCursor, remotePresence, setRemoteCursors } from "./presence";

export type CaretTarget = number | "start" | "end";

export interface BlockEditorProps {
  doc: LoroDoc;
  treeId: TreeID;
  caret: CaretTarget;
  undoManager: UndoManager;
  remoteCursors: RemoteCursor[];
  placeholder?: string;
  onSplit(offset: number): void;
  onMergeBackward(): void;
  onIndent(): void;
  onOutdent(): void;
  onMove(direction: "up" | "down"): void;
  onEscape(): void;
  onSelection(anchor: number, head: number): void;
}

export function BlockEditor(props: BlockEditorProps) {
  const host = useRef<HTMLDivElement>(null);
  const viewRef = useRef<EditorView | null>(null);
  const propsRef = useRef(props);
  useLayoutEffect(() => {
    propsRef.current = props;
  });
  const { doc, treeId, undoManager } = props;

  useEffect(() => {
    const parent = host.current;
    if (!parent) return;
    const p = propsRef.current;
    const text = contentText(doc, treeId);
    const initial = text?.toString() ?? "";
    const custom = keymap.of([
      {
        key: "Enter",
        run: (v) => {
          propsRef.current.onSplit(v.state.selection.main.head);
          return true;
        },
      },
      { key: "Shift-Enter", run: insertNewlineAndIndent },
      {
        key: "Backspace",
        run: (v) => {
          const sel = v.state.selection.main;
          if (sel.empty && sel.head === 0) {
            propsRef.current.onMergeBackward();
            return true;
          }
          return false;
        },
      },
      {
        key: "Tab",
        run: () => {
          propsRef.current.onIndent();
          return true;
        },
      },
      {
        key: "Shift-Tab",
        run: () => {
          propsRef.current.onOutdent();
          return true;
        },
      },
      {
        key: "ArrowUp",
        run: (v) => {
          const head = v.state.selection.main.head;
          if (v.state.doc.lineAt(head).number === 1) {
            propsRef.current.onMove("up");
            return true;
          }
          return false;
        },
      },
      {
        key: "ArrowDown",
        run: (v) => {
          const head = v.state.selection.main.head;
          if (v.state.doc.lineAt(head).number === v.state.doc.lines) {
            propsRef.current.onMove("down");
            return true;
          }
          return false;
        },
      },
      {
        key: "Escape",
        run: () => {
          propsRef.current.onEscape();
          return true;
        },
      },
      {
        key: "Mod-z",
        run: () => {
          if (undoManager.canUndo()) undoManager.undo();
          return true;
        },
      },
      {
        key: "Mod-Shift-z",
        run: () => {
          if (undoManager.canRedo()) undoManager.redo();
          return true;
        },
      },
      {
        key: "Mod-y",
        run: () => {
          if (undoManager.canRedo()) undoManager.redo();
          return true;
        },
      },
    ]);
    const state = EditorState.create({
      doc: initial,
      extensions: [
        Prec.highest(custom),
        keymap.of(defaultKeymap),
        markdown({ base: markdownLanguage }),
        syntaxHighlighting(defaultHighlightStyle, { fallback: true }),
        EditorView.lineWrapping,
        editorTheme,
        cmPlaceholder(p.placeholder ?? ""),
        loroTextBinding(doc, () => contentText(doc, treeId), `cm:${treeId}`),
        remotePresence(),
        EditorView.updateListener.of((u) => {
          if (u.selectionSet || u.docChanged || u.focusChanged) {
            const sel = u.state.selection.main;
            if (u.view.hasFocus) propsRef.current.onSelection(sel.anchor, sel.head);
          }
        }),
      ],
    });
    const view = new EditorView({ state, parent });
    viewRef.current = view;
    const len = view.state.doc.length;
    const pos = p.caret === "end" ? len : p.caret === "start" ? 0 : Math.max(0, Math.min(p.caret, len));
    view.dispatch({ selection: EditorSelection.cursor(pos) });
    view.focus();
    view.dispatch({ effects: setRemoteCursors.of(p.remoteCursors) });
    return () => {
      view.destroy();
      viewRef.current = null;
    };
  }, [doc, treeId, undoManager]);

  useEffect(() => {
    viewRef.current?.dispatch({ effects: setRemoteCursors.of(props.remoteCursors) });
  }, [props.remoteCursors]);

  return <div ref={host} className="editor-host" data-testid="block-editor" />;
}
