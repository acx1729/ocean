// The block tree inside a page doc, laid out exactly as the server writes it:
// a root map "meta" (title, icon, format, type_id, journal_date, props) and a
// root tree "blocks" whose node data holds id, type_id, created_at, created_by,
// a "props" map and a "content" text with the block's Markdown source.

import { LoroDoc, LoroMap, LoroText, type TreeID } from "loro-crdt";
import { uuidv7 } from "@kb/sync-client";

export const BLOCKS = "blocks";
export const META = "meta";

export interface BlockNode {
  treeId: TreeID;
  id: string;
  typeId: string;
  content: string;
  props: Record<string, unknown>;
  portalDocId: string;
  parent: TreeID | null;
  depth: number;
  children: BlockNode[];
}

export interface PageMeta {
  title: string;
  icon: string;
  format: string;
  typeId: string;
  journalDate: string;
}

interface TreeValue {
  id: TreeID;
  parent?: TreeID | null;
  index?: number;
  meta?: Record<string, unknown> | null;
  children?: TreeValue[];
}

function str(v: unknown): string {
  return typeof v === "string" ? v : "";
}

export function blocksTree(doc: LoroDoc) {
  return doc.getTree(BLOCKS);
}

export function readMeta(doc: LoroDoc): PageMeta {
  const m = doc.getMap(META).toJSON() as Record<string, unknown>;
  return {
    title: str(m.title),
    icon: str(m.icon),
    format: str(m.format),
    typeId: str(m.type_id),
    journalDate: str(m.journal_date),
  };
}

export function setMetaTitle(doc: LoroDoc, title: string): void {
  const m = doc.getMap(META);
  if (m.get("title") === title) return;
  m.set("title", title);
  doc.commit();
}

/** The block hierarchy in sibling order. */
export function readBlocks(doc: LoroDoc): BlockNode[] {
  // toJSON() yields plain meta objects (toArray() hands out live LoroMap handles).
  const raw = blocksTree(doc).toJSON() as TreeValue[];
  const conv = (nodes: TreeValue[], parent: TreeID | null, depth: number): BlockNode[] =>
    nodes.map((n) => {
      const meta = n.meta ?? {};
      return {
        treeId: n.id,
        id: str(meta.id),
        typeId: str(meta.type_id),
        content: str(meta.content),
        props: (meta.props as Record<string, unknown> | undefined) ?? {},
        portalDocId: str(meta.portal_doc_id),
        parent,
        depth,
        children: conv(n.children ?? [], n.id, depth + 1),
      };
    });
  return conv(raw, null, 0);
}

/** Pre-order flattening (the visual order of an outline). */
export function flatten(nodes: BlockNode[], out: BlockNode[] = []): BlockNode[] {
  for (const n of nodes) {
    out.push(n);
    flatten(n.children, out);
  }
  return out;
}

export function findBlock(nodes: BlockNode[], treeId: TreeID): BlockNode | null {
  for (const n of nodes) {
    if (n.treeId === treeId) return n;
    const hit = findBlock(n.children, treeId);
    if (hit) return hit;
  }
  return null;
}

/** The block's Markdown text container (null when the node has none yet). */
export function contentText(doc: LoroDoc, treeId: TreeID): LoroText | null {
  const node = blocksTree(doc).getNodeByID(treeId);
  if (!node) return null;
  const c = node.data.get("content");
  return c instanceof LoroText ? c : null;
}

export interface CreateBlockOptions {
  parent?: TreeID | null;
  /** Position among the siblings; appended when omitted. */
  index?: number;
  content?: string;
  typeId?: string;
  createdBy?: string;
  id?: string;
  /** Skip the commit (callers batching several changes). */
  noCommit?: boolean;
}

export function createBlock(doc: LoroDoc, opts: CreateBlockOptions = {}): { treeId: TreeID; id: string } {
  const t = blocksTree(doc);
  const parent = opts.parent ?? undefined;
  const node = opts.index === undefined ? t.createNode(parent) : t.createNode(parent, opts.index);
  const id = opts.id ?? uuidv7();
  node.data.set("id", id);
  if (opts.typeId) node.data.set("type_id", opts.typeId);
  node.data.set("created_at", new Date().toISOString());
  if (opts.createdBy) node.data.set("created_by", opts.createdBy);
  node.data.setContainer("props", new LoroMap());
  const text = node.data.setContainer("content", new LoroText());
  if (opts.content) text.insert(0, opts.content);
  if (!opts.noCommit) doc.commit();
  return { treeId: node.id, id };
}

export function moveBlock(doc: LoroDoc, treeId: TreeID, parent: TreeID | null, index?: number): void {
  blocksTree(doc).move(treeId, parent ?? undefined, index);
  doc.commit();
}

export function deleteBlock(doc: LoroDoc, treeId: TreeID): void {
  blocksTree(doc).delete(treeId);
  doc.commit();
}

function siblings(tree: BlockNode[], parent: TreeID | null): BlockNode[] {
  if (parent === null) return tree;
  return findBlock(tree, parent)?.children ?? [];
}

/** Makes the block the last child of its previous sibling. */
export function indentBlock(doc: LoroDoc, tree: BlockNode[], treeId: TreeID): boolean {
  const node = findBlock(tree, treeId);
  if (!node) return false;
  const sibs = siblings(tree, node.parent);
  const i = sibs.findIndex((s) => s.treeId === treeId);
  if (i <= 0) return false;
  const prev = sibs[i - 1]!;
  moveBlock(doc, treeId, prev.treeId, prev.children.length);
  return true;
}

/** Moves the block right after its parent, taking its following siblings along as children. */
export function outdentBlock(doc: LoroDoc, tree: BlockNode[], treeId: TreeID): boolean {
  const node = findBlock(tree, treeId);
  if (!node || node.parent === null) return false;
  const parent = findBlock(tree, node.parent);
  if (!parent) return false;
  const sibs = parent.children;
  const i = sibs.findIndex((s) => s.treeId === treeId);
  const following = sibs.slice(i + 1);
  const grand = siblings(tree, parent.parent);
  const pi = grand.findIndex((s) => s.treeId === parent.treeId);
  const t = blocksTree(doc);
  t.move(treeId, parent.parent ?? undefined, pi + 1);
  let k = node.children.length;
  for (const f of following) t.move(f.treeId, treeId, k++);
  doc.commit();
  return true;
}

/**
 * Splits a block at a UTF-16 offset: the text after the offset moves to a new
 * block right below (as the first child when the block has children, else as
 * the next sibling). Returns the new block.
 */
export function splitBlock(
  doc: LoroDoc,
  tree: BlockNode[],
  treeId: TreeID,
  offset: number,
  createdBy?: string,
): TreeID | null {
  const node = findBlock(tree, treeId);
  const text = contentText(doc, treeId);
  if (!node || !text) return null;
  const full = text.toString();
  const at = Math.max(0, Math.min(offset, full.length));
  const rest = full.slice(at);
  if (rest.length > 0) text.delete(at, full.length - at);
  let created: { treeId: TreeID };
  if (node.children.length > 0) {
    created = createBlock(doc, { parent: treeId, index: 0, content: rest, createdBy, noCommit: true });
  } else {
    const sibs = siblings(tree, node.parent);
    const i = sibs.findIndex((s) => s.treeId === treeId);
    created = createBlock(doc, {
      parent: node.parent,
      index: i + 1,
      content: rest,
      createdBy,
      noCommit: true,
    });
  }
  doc.commit();
  return created.treeId;
}

/** The block shown right above in the outline (deepest last descendant of the previous sibling, else the parent). */
export function previousVisible(tree: BlockNode[], treeId: TreeID): BlockNode | null {
  const flat = flatten(tree);
  const i = flat.findIndex((b) => b.treeId === treeId);
  return i > 0 ? flat[i - 1]! : null;
}

export function nextVisible(tree: BlockNode[], treeId: TreeID): BlockNode | null {
  const flat = flatten(tree);
  const i = flat.findIndex((b) => b.treeId === treeId);
  return i >= 0 && i + 1 < flat.length ? flat[i + 1]! : null;
}

/**
 * Merges a block into the one above: its text is appended there and its
 * children are re-parented under the target. Returns the target and the offset
 * where the merged text starts (the caret position).
 */
export function mergeIntoPrevious(
  doc: LoroDoc,
  tree: BlockNode[],
  treeId: TreeID,
): { treeId: TreeID; offset: number } | null {
  const node = findBlock(tree, treeId);
  const prev = previousVisible(tree, treeId);
  if (!node || !prev) return null;
  const target = contentText(doc, prev.treeId);
  if (!target) return null;
  const offset = target.toString().length;
  if (node.content.length > 0) target.insert(offset, node.content);
  const t = blocksTree(doc);
  let k = prev.children.length;
  for (const ch of node.children) t.move(ch.treeId, prev.treeId, k++);
  t.delete(treeId);
  doc.commit();
  return { treeId: prev.treeId, offset };
}

/** Ensures a page has at least one block to type into. */
export function ensureFirstBlock(doc: LoroDoc, createdBy?: string): TreeID | null {
  const roots = blocksTree(doc).roots();
  if (roots.length > 0) return null;
  return createBlock(doc, { createdBy }).treeId;
}

export function newDoc(): LoroDoc {
  const doc = new LoroDoc();
  doc.getTree(BLOCKS).enableFractionalIndex(0);
  return doc;
}
