import { describe, expect, it } from "vitest";
import {
  createBlock,
  flatten,
  indentBlock,
  mergeIntoPrevious,
  newDoc,
  outdentBlock,
  readBlocks,
  readMeta,
  setMetaTitle,
  splitBlock,
} from "./blocks";

describe("blocks", () => {
  it("creates blocks with the server's node layout", () => {
    const doc = newDoc();
    const { treeId, id } = createBlock(doc, { content: "hello", createdBy: "did:key:z1" });
    const node = doc.getTree("blocks").getNodeByID(treeId)!;
    const data = node.data.toJSON() as Record<string, unknown>;
    expect(data.id).toBe(id);
    expect(data.content).toBe("hello");
    expect(data.props).toEqual({});
    expect(data.created_by).toBe("did:key:z1");
    expect(typeof data.created_at).toBe("string");
    const tree = readBlocks(doc);
    expect(tree).toHaveLength(1);
    expect(tree[0]!.content).toBe("hello");
    expect(tree[0]!.id).toBe(id);
  });

  it("indents, outdents and keeps sibling order", () => {
    const doc = newDoc();
    const a = createBlock(doc, { content: "a" }).treeId;
    const b = createBlock(doc, { content: "b" }).treeId;
    const c = createBlock(doc, { content: "c" }).treeId;
    expect(indentBlock(doc, readBlocks(doc), b)).toBe(true);
    let tree = readBlocks(doc);
    expect(tree.map((n) => n.content)).toEqual(["a", "c"]);
    expect(tree[0]!.children.map((n) => n.content)).toEqual(["b"]);
    expect(indentBlock(doc, tree, c)).toBe(true);
    tree = readBlocks(doc);
    expect(tree[0]!.children.map((n) => n.content)).toEqual(["b", "c"]);
    // Outdenting b takes c along as its child.
    expect(outdentBlock(doc, tree, b)).toBe(true);
    tree = readBlocks(doc);
    expect(tree.map((n) => n.content)).toEqual(["a", "b"]);
    expect(tree[1]!.children.map((n) => n.content)).toEqual(["c"]);
    expect(flatten(tree).map((n) => n.content)).toEqual(["a", "b", "c"]);
    expect(indentBlock(doc, tree, a)).toBe(false);
  });

  it("splits and merges blocks around the caret", () => {
    const doc = newDoc();
    const a = createBlock(doc, { content: "hello world" }).treeId;
    const created = splitBlock(doc, readBlocks(doc), a, 5)!;
    let tree = readBlocks(doc);
    expect(tree.map((n) => n.content)).toEqual(["hello", " world"]);
    expect(tree[1]!.treeId).toBe(created);
    const merged = mergeIntoPrevious(doc, tree, created)!;
    expect(merged.treeId).toBe(a);
    expect(merged.offset).toBe(5);
    tree = readBlocks(doc);
    expect(tree.map((n) => n.content)).toEqual(["hello world"]);
  });

  it("stores the title in meta", () => {
    const doc = newDoc();
    setMetaTitle(doc, "Notes");
    expect(readMeta(doc).title).toBe("Notes");
    expect(doc.getMap("meta").toJSON()).toEqual({ title: "Notes" });
  });
});
