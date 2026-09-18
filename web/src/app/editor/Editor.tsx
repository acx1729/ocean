import type { DocHandle, PresenceState } from "@kb/sync-client";
import { Cursor, UndoManager, type LoroDoc, type TreeID } from "loro-crdt";
import { useCallback, useEffect, useMemo, useState } from "react";
import {
  type BlockNode,
  contentText,
  createBlock,
  ensureFirstBlock,
  flatten,
  indentBlock,
  mergeIntoPrevious,
  nextVisible,
  outdentBlock,
  previousVisible,
  readBlocks,
  splitBlock,
} from "../../lib/blocks";
import { renderMarkdown } from "../../lib/markdown";
import { useDocVersion, useHandleSync } from "../hooks";
import { Avatar } from "../components";
import { BlockEditor, type CaretTarget } from "./BlockEditor";
import type { RemoteCursor } from "./presence";

interface Focus {
  treeId: TreeID;
  caret: CaretTarget;
}

export interface EditorProps {
  handle: DocHandle;
  me: { did: string; name: string; color: string };
  onNavigate(target: { kind: "page" | "tag"; value: string }): void;
}

interface PeerView {
  peer: string;
  state: PresenceState;
}

export function Editor({ handle, me, onNavigate }: EditorProps) {
  const doc = handle.doc;
  const version = useDocVersion(doc);
  useHandleSync(handle);
  const [requestedFocus, setFocus] = useState<Focus | null>(null);
  const [peers, setPeers] = useState<PeerView[]>(() =>
    [...handle.awareness.peers().entries()].map(([peer, state]) => ({ peer, state })),
  );
  const undoManager = useMemo(() => new UndoManager(doc, { excludeOriginPrefixes: ["sys:"] }), [doc]);
  const tree = useMemo(() => {
    void version; // re-read after every commit
    return readBlocks(doc);
  }, [doc, version]);
  const flat = useMemo(() => flatten(tree), [tree]);
  // The focus is dropped when its block disappears (deleted remotely, merged).
  const focus =
    requestedFocus && flat.some((b) => b.treeId === requestedFocus.treeId) ? requestedFocus : null;

  // Presence: publish who we are as soon as the doc opens; the cursor follows the selection.
  useEffect(() => {
    handle.awareness.setLocal({ name: me.name, color: me.color, cursor: null });
    return handle.awareness.subscribe((map) =>
      setPeers([...map.entries()].map(([peer, state]) => ({ peer, state }))),
    );
  }, [handle, me.name, me.color]);

  // A page always offers one block to type into once its server state is known.
  useEffect(() => {
    if (flat.length === 0 && (handle.synced || handle.lastSeq > 0)) {
      const created = ensureFirstBlock(doc, me.did);
      if (created) queueMicrotask(() => setFocus({ treeId: created, caret: 0 }));
    }
  }, [doc, flat.length, handle, handle.synced, handle.lastSeq, me.did, version]);

  const publishCursor = useCallback(
    (treeId: TreeID | null, anchor: number, head: number) => {
      const local = handle.awareness.getLocal() ?? { name: me.name, color: me.color, cursor: null };
      if (!treeId) {
        if (local.cursor !== null) handle.awareness.setLocal({ ...local, cursor: null });
        return;
      }
      const block = flat.find((b) => b.treeId === treeId);
      const text = contentText(doc, treeId);
      if (!block || !text) return;
      try {
        const a = text.getCursor(anchor, 0)?.encode();
        const h = text.getCursor(head, 0)?.encode();
        if (!a || !h) return;
        handle.awareness.setLocal({ ...local, cursor: { blockId: block.id, anchor: a, head: h } });
      } catch {
        // cursor positions can be transiently invalid during a remote import
      }
    },
    [doc, flat, handle, me.color, me.name],
  );

  const remoteCursorsFor = useCallback(
    (block: BlockNode): RemoteCursor[] => {
      const out: RemoteCursor[] = [];
      for (const { peer, state } of peers) {
        const c = state.cursor;
        if (!c || c.blockId !== block.id) continue;
        try {
          const anchor = doc.getCursorPos(Cursor.decode(c.anchor))?.offset;
          const head = doc.getCursorPos(Cursor.decode(c.head))?.offset;
          if (anchor === undefined || head === undefined) continue;
          out.push({ peer, name: state.name, color: state.color, anchor, head });
        } catch {
          // stale cursor bytes
        }
      }
      return out;
    },
    [doc, peers],
  );

  const actions = useMemo(
    () => ({
      split(treeId: TreeID, offset: number) {
        const created = splitBlock(doc, readBlocks(doc), treeId, offset, me.did);
        if (created) setFocus({ treeId: created, caret: 0 });
      },
      mergeBackward(treeId: TreeID) {
        const current = readBlocks(doc);
        const all = flatten(current);
        const block = all.find((b) => b.treeId === treeId);
        if (!block) return;
        if (all.length === 1) return; // never delete the last block
        const merged = mergeIntoPrevious(doc, current, treeId);
        if (merged) setFocus({ treeId: merged.treeId, caret: merged.offset });
      },
      indent(treeId: TreeID) {
        indentBlock(doc, readBlocks(doc), treeId);
      },
      outdent(treeId: TreeID) {
        outdentBlock(doc, readBlocks(doc), treeId);
      },
      move(treeId: TreeID, direction: "up" | "down") {
        const current = readBlocks(doc);
        const target = direction === "up" ? previousVisible(current, treeId) : nextVisible(current, treeId);
        if (target) setFocus({ treeId: target.treeId, caret: direction === "up" ? "end" : "start" });
      },
      escape() {
        setFocus(null);
        publishCursor(null, 0, 0);
      },
      addAtEnd() {
        const current = flatten(readBlocks(doc));
        const last = current[current.length - 1];
        if (last && last.content.trim() === "" && last.children.length === 0) {
          setFocus({ treeId: last.treeId, caret: "end" });
          return;
        }
        const created = createBlock(doc, { createdBy: me.did });
        setFocus({ treeId: created.treeId, caret: 0 });
      },
    }),
    [doc, me.did, publishCursor],
  );

  return (
    <div className="blocks" data-testid="blocks">
      {flat.map((block) => {
        const focused = focus?.treeId === block.treeId;
        const here = peers.filter((p) => p.state.cursor?.blockId === block.id);
        return (
          <div
            key={block.treeId}
            className={`block${focused ? " focused" : ""}`}
            style={{ paddingLeft: block.depth * 24 }}
            data-testid="block"
            data-block-id={block.id}
          >
            <span className="bullet" aria-hidden="true">
              •
            </span>
            <div className="body">
              {focused ? (
                <BlockEditor
                  doc={doc}
                  treeId={block.treeId}
                  caret={focus.caret}
                  undoManager={undoManager}
                  remoteCursors={remoteCursorsFor(block)}
                  placeholder="Type Markdown…"
                  onSplit={(offset) => actions.split(block.treeId, offset)}
                  onMergeBackward={() => actions.mergeBackward(block.treeId)}
                  onIndent={() => actions.indent(block.treeId)}
                  onOutdent={() => actions.outdent(block.treeId)}
                  onMove={(d) => actions.move(block.treeId, d)}
                  onEscape={() => actions.escape()}
                  onSelection={(a, h) => publishCursor(block.treeId, a, h)}
                />
              ) : (
                <div
                  className={`preview${block.content.trim() === "" ? " placeholder" : ""}`}
                  onMouseDown={(e) => {
                    if ((e.target as HTMLElement).closest("a")) return;
                    e.preventDefault();
                    setFocus({ treeId: block.treeId, caret: "end" });
                  }}
                >
                  {block.content.trim() === ""
                    ? "Empty block"
                    : renderMarkdown(block.content, { onNavigate })}
                </div>
              )}
            </div>
            {here.length > 0 && (
              <div className="peers">
                {here.map((p) => (
                  <Avatar key={p.peer} name={p.state.name} color={p.state.color} />
                ))}
              </div>
            )}
          </div>
        );
      })}
      <div
        className="add-block"
        onMouseDown={(e) => {
          e.preventDefault();
          actions.addAtEnd();
        }}
      >
        + Add a block
      </div>
    </div>
  );
}

export type { LoroDoc };
