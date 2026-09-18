// Block Markdown rendering: remark (GFM) → rehype → sanitized hast → React.
// Wiki links ([[Title]]) and #tags become kb: links the app routes itself.

import { toJsxRuntime } from "hast-util-to-jsx-runtime";
import type { Root as HastRoot } from "hast";
import type { Root as MdastRoot, PhrasingContent, Text } from "mdast";
import { createElement, Fragment, type AnchorHTMLAttributes, type ReactNode } from "react";
import { jsx, jsxs } from "react/jsx-runtime";
import rehypeSanitize, { defaultSchema, type Options as SanitizeSchema } from "rehype-sanitize";
import remarkGfm from "remark-gfm";
import remarkParse from "remark-parse";
import remarkRehype from "remark-rehype";
import { unified } from "unified";
import { visit } from "unist-util-visit";

export const WIKI_LINK_PROTOCOL = "kb:";

const LINK_RE = /\[\[([^\]\n]+?)\]\]|(^|[\s(])#([\p{L}\p{N}_/-]+)/gu;

/** Parses [[Title]] and #tag inside text nodes into link nodes. */
function remarkKbLinks() {
  return (tree: MdastRoot) => {
    visit(tree, "text", (node: Text, index, parent) => {
      if (!parent || index === undefined) return;
      if (parent.type === "link" || parent.type === "linkReference") return;
      const value = node.value;
      LINK_RE.lastIndex = 0;
      const out: PhrasingContent[] = [];
      let last = 0;
      for (let m = LINK_RE.exec(value); m; m = LINK_RE.exec(value)) {
        const start = m.index + (m[3] ? m[2]!.length : 0);
        if (start > last) out.push({ type: "text", value: value.slice(last, start) });
        if (m[1]) {
          const title = m[1].trim();
          out.push({
            type: "link",
            url: `${WIKI_LINK_PROTOCOL}page/${encodeURIComponent(title)}`,
            children: [{ type: "text", value: title }],
          });
        } else {
          const tag = m[3]!;
          out.push({
            type: "link",
            url: `${WIKI_LINK_PROTOCOL}tag/${encodeURIComponent(tag)}`,
            children: [{ type: "text", value: `#${tag}` }],
          });
        }
        last = m.index + m[0].length;
      }
      if (out.length === 0) return;
      if (last < value.length) out.push({ type: "text", value: value.slice(last) });
      (parent.children as PhrasingContent[]).splice(index, 1, ...out);
      return index + out.length;
    });
  };
}

const schema: SanitizeSchema = {
  ...defaultSchema,
  protocols: { ...defaultSchema.protocols, href: [...(defaultSchema.protocols?.href ?? []), "kb"] },
  attributes: {
    ...defaultSchema.attributes,
    code: [...(defaultSchema.attributes?.code ?? []), ["className", /^language-./]],
    input: [
      ...(defaultSchema.attributes?.input ?? []),
      ["type", "checkbox"],
      ["checked", true],
      ["disabled", true],
    ],
  },
};

const processor = unified()
  .use(remarkParse)
  .use(remarkGfm)
  .use(remarkKbLinks)
  .use(remarkRehype)
  .use(rehypeSanitize, schema);

export interface RenderOptions {
  /** Called for kb: links; the default lets the browser handle the click. */
  onNavigate?: (target: { kind: "page" | "tag"; value: string }) => void;
}

export function parseKbLink(href: string): { kind: "page" | "tag"; value: string } | null {
  if (!href.startsWith(WIKI_LINK_PROTOCOL)) return null;
  const rest = href.slice(WIKI_LINK_PROTOCOL.length);
  const slash = rest.indexOf("/");
  if (slash < 0) return null;
  const kind = rest.slice(0, slash);
  if (kind !== "page" && kind !== "tag") return null;
  return { kind, value: decodeURIComponent(rest.slice(slash + 1)) };
}

export function markdownToHast(source: string): HastRoot {
  const mdast = processor.parse(source);
  return processor.runSync(mdast) as HastRoot;
}

type AnchorProps = AnchorHTMLAttributes<HTMLAnchorElement> & { node?: unknown };

export function renderMarkdown(source: string, opts: RenderOptions = {}): ReactNode {
  const hast = markdownToHast(source);
  const Anchor = ({ node: _node, ...props }: AnchorProps) => {
    const href = props.href ?? "";
    const kb = parseKbLink(href);
    if (kb) {
      return createElement(
        "a",
        {
          ...props,
          href:
            kb.kind === "page"
              ? `?page=${encodeURIComponent(kb.value)}`
              : `?tag=${encodeURIComponent(kb.value)}`,
          className: kb.kind === "page" ? "kb-link" : "kb-tag",
          onClick: (e: { preventDefault(): void }) => {
            if (opts.onNavigate) {
              e.preventDefault();
              opts.onNavigate(kb);
            }
          },
        },
        props.children,
      );
    }
    return createElement("a", { ...props, target: "_blank", rel: "noreferrer noopener" }, props.children);
  };
  return toJsxRuntime(hast, { Fragment, jsx, jsxs, components: { a: Anchor } });
}

/** Plain text of a Markdown source, used for outlines and titles. */
export function markdownToText(source: string): string {
  return source
    .replace(/```[\s\S]*?```/g, " ")
    .replace(/`([^`]*)`/g, "$1")
    .replace(/!\[([^\]]*)\]\([^)]*\)/g, "$1")
    .replace(/\[\[([^\]]+)\]\]/g, "$1")
    .replace(/\[([^\]]*)\]\([^)]*\)/g, "$1")
    .replace(/^#{1,6}\s+/gm, "")
    .replace(/^\s*[-*+]\s+(\[[ xX]\]\s+)?/gm, "")
    .replace(/[*_~]+/g, "")
    .trim();
}
