//! Whole-document state rendered as JSON for the Go side.
//!
//! Shape:
//!
//! ```json
//! {
//!   "meta": { "title": "...", "props": { ... }, ... },
//!   "blocks": [
//!     { "id": "<counter>@<peer>", "parent": "<tree id>" | null, "index": 0,
//!       "fractional_index": "<hex>", "meta": { "id": "<uuid>", "props": {...}, "content": "...", ... } }
//!   ]
//! }
//! ```
//!
//! Blocks are listed in tree order: roots by index, then each subtree depth-first
//! with siblings by index. Deleted nodes are not listed.

use loro::LoroDoc;
use serde_json::{json, Map, Value};

use crate::{BLOCKS, META};

/// Fields copied from Loro's hierarchical tree value into each flat block entry.
const NODE_FIELDS: [&str; 5] = ["id", "parent", "index", "fractional_index", "meta"];

/// Serializes the document state.
pub fn state_json(doc: &LoroDoc) -> Result<Vec<u8>, String> {
    let meta = Value::from(doc.get_map(META).get_deep_value());
    // Loro resolves every node's data map (and the containers nested in it)
    // when asked for the value with meta; this crate only flattens the result.
    let forest = Value::from(doc.get_tree(BLOCKS).get_value_with_meta());
    let mut blocks = Vec::new();
    flatten(&forest, &mut blocks)?;
    serde_json::to_vec(&json!({ "meta": meta, "blocks": blocks }))
        .map_err(|e| format!("encode state: {e}"))
}

/// Appends `level` and its descendants to `out` in depth-first order.
fn flatten(level: &Value, out: &mut Vec<Value>) -> Result<(), String> {
    let nodes = level
        .as_array()
        .ok_or_else(|| String::from("tree level is not a list"))?;
    for node in nodes {
        let fields = node
            .as_object()
            .ok_or_else(|| String::from("tree node is not an object"))?;
        let mut flat = Map::with_capacity(NODE_FIELDS.len());
        for name in NODE_FIELDS {
            flat.insert(
                name.to_string(),
                fields.get(name).cloned().unwrap_or(Value::Null),
            );
        }
        out.push(Value::Object(flat));
        if let Some(children) = fields.get("children") {
            flatten(children, out)?;
        }
    }
    Ok(())
}
