//! Structured operations: parsing and application.
//!
//! A batch is a JSON array of ops. Every op is validated (see `validate`)
//! against a shadow model of the block tree before anything is mutated, so a
//! rejected batch leaves the document untouched. Nodes created earlier in the
//! batch can be referenced by later ops through the placeholder `$<uuid>`.

use std::collections::HashMap;
use std::fmt;

use loro::{
    Container, ExportMode, LoroDoc, LoroMap, LoroText, LoroTree, LoroValue, TreeID, TreeParentId,
    UpdateOptions, ValueOrContainer,
};
use serde::Deserialize;
use serde_json::{json, Map, Value};

use crate::ffi::Failure;
use crate::validate::{validate, NodeRef};
use crate::{BLOCKS, META};

const KEY_ID: &str = "id";
const KEY_TYPE_ID: &str = "type_id";
const KEY_CREATED_AT: &str = "created_at";
const KEY_CREATED_BY: &str = "created_by";
const KEY_PROPS: &str = "props";
const KEY_CONTENT: &str = "content";

/// Outcome of a successful batch.
pub struct Applied {
    /// Loro update covering exactly this batch.
    pub update: Vec<u8>,
    /// JSON `{"created": {"<uuid>": "<tree id>"}, "changed": bool}`.
    pub result: Vec<u8>,
}

fn default_index() -> i64 {
    -1
}

/// Wire form of one op, exactly as the Go side encodes it.
#[derive(Deserialize)]
#[serde(tag = "op")]
enum RawOp {
    #[serde(rename = "meta.set")]
    MetaSet { key: String, value: Value },
    #[serde(rename = "meta.del")]
    MetaDel { key: String },
    #[serde(rename = "meta.props.set")]
    MetaPropsSet { key: String, value: Value },
    #[serde(rename = "meta.props.del")]
    MetaPropsDel { key: String },
    #[serde(rename = "tree.create")]
    TreeCreate {
        id: String,
        #[serde(default)]
        parent: String,
        #[serde(default = "default_index")]
        index: i64,
        type_id: Option<String>,
        created_at: Option<String>,
        created_by: Option<String>,
        content: Option<String>,
        props: Option<Map<String, Value>>,
    },
    #[serde(rename = "tree.move")]
    TreeMove {
        node: String,
        #[serde(default)]
        parent: String,
        #[serde(default = "default_index")]
        index: i64,
    },
    #[serde(rename = "tree.delete")]
    TreeDelete { node: String },
    #[serde(rename = "node.set")]
    NodeSet { node: String, key: String, value: Value },
    #[serde(rename = "node.del")]
    NodeDel { node: String, key: String },
    #[serde(rename = "node.text")]
    NodeText { node: String, text: String },
    #[serde(rename = "node.props.set")]
    NodePropsSet { node: String, key: String, value: Value },
    #[serde(rename = "node.props.del")]
    NodePropsDel { node: String, key: String },
    #[serde(rename = "node.props.mapset")]
    NodePropsMapSet { node: String, key: String, subkey: String, value: Value },
    #[serde(rename = "node.props.mapdel")]
    NodePropsMapDel { node: String, key: String, subkey: String },
}

/// Optional initial data of a created node.
pub(crate) struct CreateInit {
    pub(crate) type_id: Option<String>,
    pub(crate) created_at: Option<String>,
    pub(crate) created_by: Option<String>,
    pub(crate) content: Option<String>,
    pub(crate) props: Option<Map<String, Value>>,
}

/// A parsed op with resolved reference syntax.
pub(crate) enum Op {
    MetaSet { key: String, value: Value },
    MetaDel { key: String },
    MetaPropsSet { key: String, value: Value },
    MetaPropsDel { key: String },
    TreeCreate { id: String, parent: Option<NodeRef>, index: i64, init: CreateInit },
    TreeMove { node: NodeRef, parent: Option<NodeRef>, index: i64 },
    TreeDelete { node: NodeRef },
    NodeSet { node: NodeRef, key: String, value: Value },
    NodeDel { node: NodeRef, key: String },
    NodeText { node: NodeRef, text: String },
    NodePropsSet { node: NodeRef, key: String, value: Value },
    NodePropsDel { node: NodeRef, key: String },
    NodePropsMapSet { node: NodeRef, key: String, subkey: String, value: Value },
    NodePropsMapDel { node: NodeRef, key: String, subkey: String },
}

impl Op {
    /// The wire name of the op, for messages.
    pub(crate) fn name(&self) -> &'static str {
        match self {
            Op::MetaSet { .. } => "meta.set",
            Op::MetaDel { .. } => "meta.del",
            Op::MetaPropsSet { .. } => "meta.props.set",
            Op::MetaPropsDel { .. } => "meta.props.del",
            Op::TreeCreate { .. } => "tree.create",
            Op::TreeMove { .. } => "tree.move",
            Op::TreeDelete { .. } => "tree.delete",
            Op::NodeSet { .. } => "node.set",
            Op::NodeDel { .. } => "node.del",
            Op::NodeText { .. } => "node.text",
            Op::NodePropsSet { .. } => "node.props.set",
            Op::NodePropsDel { .. } => "node.props.del",
            Op::NodePropsMapSet { .. } => "node.props.mapset",
            Op::NodePropsMapDel { .. } => "node.props.mapdel",
        }
    }
}

fn lower(raw: RawOp) -> Result<Op, String> {
    Ok(match raw {
        RawOp::MetaSet { key, value } => Op::MetaSet { key, value },
        RawOp::MetaDel { key } => Op::MetaDel { key },
        RawOp::MetaPropsSet { key, value } => Op::MetaPropsSet { key, value },
        RawOp::MetaPropsDel { key } => Op::MetaPropsDel { key },
        RawOp::TreeCreate {
            id,
            parent,
            index,
            type_id,
            created_at,
            created_by,
            content,
            props,
        } => Op::TreeCreate {
            id,
            parent: NodeRef::parse_parent(&parent)?,
            index,
            init: CreateInit { type_id, created_at, created_by, content, props },
        },
        RawOp::TreeMove { node, parent, index } => Op::TreeMove {
            node: NodeRef::parse(&node)?,
            parent: NodeRef::parse_parent(&parent)?,
            index,
        },
        RawOp::TreeDelete { node } => Op::TreeDelete { node: NodeRef::parse(&node)? },
        RawOp::NodeSet { node, key, value } => {
            Op::NodeSet { node: NodeRef::parse(&node)?, key, value }
        }
        RawOp::NodeDel { node, key } => Op::NodeDel { node: NodeRef::parse(&node)?, key },
        RawOp::NodeText { node, text } => Op::NodeText { node: NodeRef::parse(&node)?, text },
        RawOp::NodePropsSet { node, key, value } => {
            Op::NodePropsSet { node: NodeRef::parse(&node)?, key, value }
        }
        RawOp::NodePropsDel { node, key } => {
            Op::NodePropsDel { node: NodeRef::parse(&node)?, key }
        }
        RawOp::NodePropsMapSet { node, key, subkey, value } => {
            Op::NodePropsMapSet { node: NodeRef::parse(&node)?, key, subkey, value }
        }
        RawOp::NodePropsMapDel { node, key, subkey } => {
            Op::NodePropsMapDel { node: NodeRef::parse(&node)?, key, subkey }
        }
    })
}

fn loro_err(e: impl fmt::Display) -> String {
    e.to_string()
}

/// The members of a relation set: an object whose values are all `true`.
fn relation_set(value: &Value) -> Option<Vec<&str>> {
    let object = value.as_object()?;
    object
        .values()
        .all(|v| v == &Value::Bool(true))
        .then(|| object.keys().map(String::as_str).collect())
}

/// Writes a property: relation sets become nested maps so concurrent adds
/// merge; every other value is stored as a plain value.
fn set_prop(props: &LoroMap, key: &str, value: &Value) -> Result<(), String> {
    match relation_set(value) {
        Some(members) => {
            let set = props.insert_container(key, LoroMap::new()).map_err(loro_err)?;
            for member in members {
                set.insert(member, true).map_err(loro_err)?;
            }
            Ok(())
        }
        None => props
            .insert(key, LoroValue::from(value.clone()))
            .map_err(loro_err),
    }
}

fn existing_map(parent: &LoroMap, key: &str) -> Option<LoroMap> {
    match parent.get(key) {
        Some(ValueOrContainer::Container(Container::Map(map))) => Some(map),
        _ => None,
    }
}

/// The map container at `key`, created (or replacing a non-map value) on demand.
fn map_at(parent: &LoroMap, key: &str) -> Result<LoroMap, String> {
    match existing_map(parent, key) {
        Some(map) => Ok(map),
        None => parent.insert_container(key, LoroMap::new()).map_err(loro_err),
    }
}

/// The text container at `key`, created (or replacing a non-text value) on demand.
fn text_at(parent: &LoroMap, key: &str) -> Result<LoroText, String> {
    match parent.get(key) {
        Some(ValueOrContainer::Container(Container::Text(text))) => Ok(text),
        _ => parent.insert_container(key, LoroText::new()).map_err(loro_err),
    }
}

/// Mutable context of one batch.
struct Batch<'a> {
    tree: &'a LoroTree,
    meta: LoroMap,
    created: HashMap<String, TreeID>,
}

impl Batch<'_> {
    fn resolve(&self, node: &NodeRef) -> Result<TreeID, String> {
        match node {
            NodeRef::Existing(id) => Ok(*id),
            NodeRef::New(uuid) => self
                .created
                .get(uuid)
                .copied()
                .ok_or_else(|| format!("unresolved placeholder ${uuid}")),
        }
    }

    fn resolve_parent(&self, parent: &Option<NodeRef>) -> Result<TreeParentId, String> {
        Ok(match parent {
            None => TreeParentId::Root,
            Some(node) => TreeParentId::Node(self.resolve(node)?),
        })
    }

    fn node_meta(&self, node: &NodeRef) -> Result<LoroMap, String> {
        let id = self.resolve(node)?;
        self.tree.get_meta(id).map_err(loro_err)
    }

    fn create(
        &mut self,
        id: &str,
        parent: &Option<NodeRef>,
        index: i64,
        init: &CreateInit,
    ) -> Result<(), String> {
        let parent = self.resolve_parent(parent)?;
        // Validation guarantees index is -1 (append) or a valid position.
        let tree_id = match usize::try_from(index) {
            Ok(at) => self.tree.create_at(parent, at),
            Err(_) => self.tree.create(parent),
        }
        .map_err(loro_err)?;
        let meta = self.tree.get_meta(tree_id).map_err(loro_err)?;
        meta.insert(KEY_ID, id).map_err(loro_err)?;
        for (key, value) in [
            (KEY_TYPE_ID, &init.type_id),
            (KEY_CREATED_AT, &init.created_at),
            (KEY_CREATED_BY, &init.created_by),
        ] {
            if let Some(value) = value {
                meta.insert(key, value.as_str()).map_err(loro_err)?;
            }
        }
        let props = meta.insert_container(KEY_PROPS, LoroMap::new()).map_err(loro_err)?;
        if let Some(object) = &init.props {
            for (key, value) in object {
                set_prop(&props, key, value)?;
            }
        }
        let content = meta.insert_container(KEY_CONTENT, LoroText::new()).map_err(loro_err)?;
        if let Some(text) = &init.content {
            if !text.is_empty() {
                content.insert(0, text).map_err(loro_err)?;
            }
        }
        self.created.insert(id.to_string(), tree_id);
        Ok(())
    }

    fn apply_one(&mut self, op: &Op) -> Result<(), String> {
        match op {
            Op::MetaSet { key, value } => self
                .meta
                .insert(key, LoroValue::from(value.clone()))
                .map_err(loro_err),
            Op::MetaDel { key } => self.meta.delete(key).map_err(loro_err),
            Op::MetaPropsSet { key, value } => set_prop(&map_at(&self.meta, KEY_PROPS)?, key, value),
            Op::MetaPropsDel { key } => match existing_map(&self.meta, KEY_PROPS) {
                Some(props) => props.delete(key).map_err(loro_err),
                None => Ok(()),
            },
            Op::TreeCreate { id, parent, index, init } => self.create(id, parent, *index, init),
            Op::TreeMove { node, parent, index } => {
                let id = self.resolve(node)?;
                let parent = self.resolve_parent(parent)?;
                match usize::try_from(*index) {
                    Ok(at) => self.tree.mov_to(id, parent, at),
                    Err(_) => self.tree.mov(id, parent),
                }
                .map_err(loro_err)
            }
            Op::TreeDelete { node } => {
                let id = self.resolve(node)?;
                self.tree.delete(id).map_err(loro_err)
            }
            Op::NodeSet { node, key, value } => self
                .node_meta(node)?
                .insert(key, LoroValue::from(value.clone()))
                .map_err(loro_err),
            Op::NodeDel { node, key } => self.node_meta(node)?.delete(key).map_err(loro_err),
            Op::NodeText { node, text } => text_at(&self.node_meta(node)?, KEY_CONTENT)?
                .update(text, UpdateOptions::default())
                .map_err(loro_err),
            Op::NodePropsSet { node, key, value } => {
                set_prop(&map_at(&self.node_meta(node)?, KEY_PROPS)?, key, value)
            }
            Op::NodePropsDel { node, key } => {
                match existing_map(&self.node_meta(node)?, KEY_PROPS) {
                    Some(props) => props.delete(key).map_err(loro_err),
                    None => Ok(()),
                }
            }
            Op::NodePropsMapSet { node, key, subkey, value } => {
                let props = map_at(&self.node_meta(node)?, KEY_PROPS)?;
                map_at(&props, key)?
                    .insert(subkey, LoroValue::from(value.clone()))
                    .map_err(loro_err)
            }
            Op::NodePropsMapDel { node, key, subkey } => {
                let set = existing_map(&self.node_meta(node)?, KEY_PROPS)
                    .and_then(|props| existing_map(&props, key));
                match set {
                    Some(set) => set.delete(subkey).map_err(loro_err),
                    None => Ok(()),
                }
            }
        }
    }
}

/// Validates and applies a JSON batch, returning the update that encodes it.
pub fn apply(doc: &LoroDoc, ops_json: &[u8]) -> Result<Applied, Failure> {
    let raw: Vec<RawOp> = serde_json::from_slice(ops_json)
        .map_err(|e| Failure::Rejected(format!("invalid ops JSON: {e}")))?;
    let ops = raw
        .into_iter()
        .enumerate()
        .map(|(i, op)| lower(op).map_err(|e| Failure::Rejected(format!("op[{i}]: {e}"))))
        .collect::<Result<Vec<_>, _>>()?;

    let tree = doc.get_tree(BLOCKS);
    if !tree.is_fractional_index_enabled() {
        tree.enable_fractional_index(0);
    }
    validate(&tree, &ops).map_err(Failure::Rejected)?;

    let pre = doc.oplog_vv();
    let mut batch = Batch { tree: &tree, meta: doc.get_map(META), created: HashMap::new() };
    for (i, op) in ops.iter().enumerate() {
        // Validation passed, so a failure here is internal to Loro; earlier ops
        // of this batch may already be in the pending transaction.
        batch
            .apply_one(op)
            .map_err(|e| Failure::Partial(format!("op[{i}] {}: {e}", op.name())))?;
    }
    doc.commit();

    let update = doc
        .export(ExportMode::updates(&pre))
        .map_err(|e| Failure::Partial(format!("export update: {e}")))?;
    let created: Map<String, Value> = batch
        .created
        .iter()
        .map(|(uuid, id)| (uuid.clone(), Value::String(id.to_string())))
        .collect();
    let changed = doc.oplog_vv() != pre;
    let result = serde_json::to_vec(&json!({ "created": created, "changed": changed }))
        .map_err(|e| Failure::Partial(format!("encode result: {e}")))?;
    Ok(Applied { update, result })
}
