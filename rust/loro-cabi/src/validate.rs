//! Up-front validation of a batch against a shadow model of the block tree.
//!
//! The shadow tree is populated lazily from the live tree and updated as the
//! batch's structural ops are simulated, so every op is checked against the
//! tree exactly as it will be when the op runs, without mutating anything.

use std::collections::{HashMap, HashSet};
use std::fmt;

use loro::{LoroTree, TreeID, TreeParentId};

use crate::ops::Op;

/// Node data keys that only dedicated ops may write.
const RESERVED_NODE_KEYS: &[&str] = &["id", "props", "content"];
/// Meta keys that only dedicated ops may write.
const RESERVED_META_KEYS: &[&str] = &["props"];

/// A node reference as written in an op.
#[derive(Clone, Debug, PartialEq, Eq, Hash)]
pub(crate) enum NodeRef {
    /// A node that exists in the document.
    Existing(TreeID),
    /// A node created earlier in the same batch, by block id.
    New(String),
}

impl NodeRef {
    /// Parses `<counter>@<peer>` or the placeholder `$<uuid>`.
    pub(crate) fn parse(text: &str) -> Result<NodeRef, String> {
        if let Some(uuid) = text.strip_prefix('$') {
            if uuid.is_empty() {
                return Err("empty placeholder reference".to_string());
            }
            return Ok(NodeRef::New(uuid.to_string()));
        }
        parse_tree_id(text).map(NodeRef::Existing)
    }

    /// Parses a parent reference; the empty string means the tree root.
    pub(crate) fn parse_parent(text: &str) -> Result<Option<NodeRef>, String> {
        if text.is_empty() {
            Ok(None)
        } else {
            NodeRef::parse(text).map(Some)
        }
    }
}

impl fmt::Display for NodeRef {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            NodeRef::Existing(id) => write!(f, "{id}"),
            NodeRef::New(uuid) => write!(f, "${uuid}"),
        }
    }
}

/// Parses Loro's textual tree id, `<counter>@<peer>`.
fn parse_tree_id(text: &str) -> Result<TreeID, String> {
    let invalid = || format!("invalid tree id {text:?}: expected <counter>@<peer>");
    let (counter, peer) = text.split_once('@').ok_or_else(invalid)?;
    let counter: i32 = counter.parse().map_err(|_| invalid())?;
    let peer: u64 = peer.parse().map_err(|_| invalid())?;
    if counter < 0 {
        return Err(invalid());
    }
    Ok(TreeID::new(peer, counter))
}

/// A model of the block tree as it will look while the batch is applied.
struct Shadow<'a> {
    tree: &'a LoroTree,
    /// Known parents (`None` = root). Filled from the live tree on demand and
    /// updated by simulated creates and moves.
    parents: HashMap<NodeRef, Option<NodeRef>>,
    /// Child lists in sibling order for the parents touched so far.
    children: HashMap<Option<NodeRef>, Vec<NodeRef>>,
    deleted: HashSet<NodeRef>,
    created: HashSet<String>,
}

impl<'a> Shadow<'a> {
    fn new(tree: &'a LoroTree) -> Self {
        Shadow {
            tree,
            parents: HashMap::new(),
            children: HashMap::new(),
            deleted: HashSet::new(),
            created: HashSet::new(),
        }
    }

    /// The parent of `node`: `None` when the node is unknown or deleted in the
    /// live tree, `Some(None)` for a root, `Some(Some(p))` otherwise.
    fn parent_of(&mut self, node: &NodeRef) -> Option<Option<NodeRef>> {
        if let Some(parent) = self.parents.get(node) {
            return Some(parent.clone());
        }
        let NodeRef::Existing(id) = node else {
            return None; // a placeholder that no earlier op created
        };
        let parent = match self.tree.parent(*id)? {
            TreeParentId::Root => None,
            TreeParentId::Node(p) => Some(NodeRef::Existing(p)),
            TreeParentId::Deleted | TreeParentId::Unexist => return None,
        };
        self.parents.insert(node.clone(), parent.clone());
        Some(parent)
    }

    /// Whether `node` is reachable from the root after the ops seen so far.
    fn is_alive(&mut self, node: &NodeRef) -> bool {
        let mut current = node.clone();
        let mut hops = 0usize;
        loop {
            if self.deleted.contains(&current) {
                return false;
            }
            match self.parent_of(&current) {
                None => return false,
                Some(None) => return true,
                Some(Some(parent)) => current = parent,
            }
            hops += 1;
            if hops > 1_000_000 {
                return false; // defensive: never spin on a corrupted parent chain
            }
        }
    }

    fn ensure_alive(&mut self, node: &NodeRef, role: &str) -> Result<(), String> {
        if self.is_alive(node) {
            Ok(())
        } else {
            Err(format!("{role} {node} does not exist or is deleted"))
        }
    }

    fn ensure_parent(&mut self, parent: &Option<NodeRef>) -> Result<(), String> {
        match parent {
            Some(p) => self.ensure_alive(p, "parent"),
            None => Ok(()),
        }
    }

    /// The child list of `parent`, read from the live tree on first use. A
    /// list is always materialized before the batch changes it, so a list not
    /// yet cached is still exactly what the live tree holds.
    fn children_of(&mut self, parent: &Option<NodeRef>) -> &mut Vec<NodeRef> {
        if !self.children.contains_key(parent) {
            let live = match parent {
                None => self.tree.roots(),
                Some(NodeRef::Existing(id)) => {
                    self.tree.children(TreeParentId::Node(*id)).unwrap_or_default()
                }
                Some(NodeRef::New(_)) => Vec::new(),
            };
            let list = live.into_iter().map(NodeRef::Existing).collect();
            self.children.insert(parent.clone(), list);
        }
        self.children.get_mut(parent).expect("child list was just inserted")
    }

    fn create(&mut self, uuid: &str, parent: &Option<NodeRef>, index: i64) -> Result<(), String> {
        if uuid.is_empty() {
            return Err("block id must not be empty".to_string());
        }
        if self.created.contains(uuid) {
            return Err(format!("block id {uuid:?} is created twice in this batch"));
        }
        self.ensure_parent(parent)?;
        let len = self.children_of(parent).len();
        let at = resolve_index(index, len)?;
        let key = NodeRef::New(uuid.to_string());
        self.children_of(parent).insert(at, key.clone());
        self.parents.insert(key, parent.clone());
        self.created.insert(uuid.to_string());
        Ok(())
    }

    /// Simulates a move with Loro's index semantics: the node leaves its old
    /// position first, so within the same parent the valid indexes are
    /// 0..=len-1 and elsewhere 0..=len.
    fn mov(&mut self, node: &NodeRef, parent: &Option<NodeRef>, index: i64) -> Result<(), String> {
        self.ensure_alive(node, "node")?;
        self.ensure_parent(parent)?;
        let mut ancestor = parent.clone();
        while let Some(candidate) = ancestor {
            if &candidate == node {
                return Err(format!("moving {node} under itself would create a cycle"));
            }
            ancestor = self.parent_of(&candidate).flatten();
        }
        let old_parent = self
            .parent_of(node)
            .ok_or_else(|| format!("node {node} does not exist or is deleted"))?;
        self.children_of(&old_parent).retain(|child| child != node);
        let len = self.children_of(parent).len();
        let at = resolve_index(index, len)?;
        self.children_of(parent).insert(at, node.clone());
        self.parents.insert(node.clone(), parent.clone());
        Ok(())
    }

    fn delete(&mut self, node: &NodeRef) -> Result<(), String> {
        self.ensure_alive(node, "node")?;
        let parent = self
            .parent_of(node)
            .ok_or_else(|| format!("node {node} does not exist or is deleted"))?;
        self.children_of(&parent).retain(|child| child != node);
        self.deleted.insert(node.clone());
        Ok(())
    }
}

/// Turns an op index into a position: -1 appends, otherwise 0..=len.
fn resolve_index(index: i64, len: usize) -> Result<usize, String> {
    if index == -1 {
        return Ok(len);
    }
    let at = usize::try_from(index).map_err(|_| format!("index {index} must be -1 or >= 0"))?;
    if at > len {
        return Err(format!("index {index} out of range (0..={len})"));
    }
    Ok(at)
}

fn check_key(key: &str, reserved: &[&str]) -> Result<(), String> {
    if key.is_empty() {
        return Err("key must not be empty".to_string());
    }
    if reserved.contains(&key) {
        return Err(format!("key {key:?} is managed by dedicated ops"));
    }
    Ok(())
}

/// Checks every op of the batch in order; the error names the failing op.
pub(crate) fn validate(tree: &LoroTree, ops: &[Op]) -> Result<(), String> {
    let mut shadow = Shadow::new(tree);
    for (i, op) in ops.iter().enumerate() {
        validate_one(&mut shadow, op).map_err(|e| format!("op[{i}] {}: {e}", op.name()))?;
    }
    Ok(())
}

fn validate_one(shadow: &mut Shadow<'_>, op: &Op) -> Result<(), String> {
    match op {
        Op::MetaSet { key, .. } | Op::MetaDel { key } => check_key(key, RESERVED_META_KEYS),
        Op::MetaPropsSet { key, .. } | Op::MetaPropsDel { key } => check_key(key, &[]),
        Op::TreeCreate { id, parent, index, init } => {
            for key in init.props.iter().flat_map(|p| p.keys()) {
                check_key(key, &[])?;
            }
            shadow.create(id, parent, *index)
        }
        Op::TreeMove { node, parent, index } => shadow.mov(node, parent, *index),
        Op::TreeDelete { node } => shadow.delete(node),
        Op::NodeSet { node, key, .. } | Op::NodeDel { node, key } => {
            check_key(key, RESERVED_NODE_KEYS)?;
            shadow.ensure_alive(node, "node")
        }
        Op::NodeText { node, .. } => shadow.ensure_alive(node, "node"),
        Op::NodePropsSet { node, key, .. } | Op::NodePropsDel { node, key } => {
            check_key(key, &[])?;
            shadow.ensure_alive(node, "node")
        }
        Op::NodePropsMapSet { node, key, subkey, .. }
        | Op::NodePropsMapDel { node, key, subkey } => {
            check_key(key, &[])?;
            check_key(subkey, &[])?;
            shadow.ensure_alive(node, "node")
        }
    }
}
