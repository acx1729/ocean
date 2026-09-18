// Package query compiles Google CEL expressions written against the block
// projection of the R1 knowledge platform into a single parameterized
// PostgreSQL statement, and evaluates the same expressions in-process for
// QueryService.Explain and for the differential test that keeps both paths
// aligned.
//
// # Environment
//
// One Env is built per (project, schema version) from a Schema; the caller
// caches it. The environment declares the symbols of section 8 of the
// specification: id, page_id, doc_id, project_id, parent_id, key, type,
// type_id, text, props.<name>, path, depth, is_page, created_at, updated_at,
// created_by, updated_by, now and the functions is_a, search, me,
// descendant_of, ancestor_of, has_edge, edges_out, edges_in, refs,
// referenced_by and in_collection. Only the has, exists and all macros are
// registered; map, filter and exists_one are rejected before type checking.
//
// # Semantics shared by both paths
//
// SQL and CEL disagree on how failures propagate: SQL uses three-valued NULL
// logic, CEL absorbs errors through && and ||. This package removes the
// difference instead of translating it, so that a predicate has exactly one
// meaning:
//
//   - A single-valued property that is absent on a block evaluates to the
//     typed zero value of its kind ("" for text, url, select and user, 0 for
//     number, false for checkbox, 0001-01-01T00:00:00Z for date), which is
//     the proto3 default the specification refers to. A multi-valued property
//     (relation, multi_select) that is absent is the empty list. has(props.x)
//     tests presence.
//   - Nullable columns (type_id, parent_block_id, key) read as "" in CEL;
//     the generated SQL is NULL-safe for every comparison against them.
//   - Every boolean SQL expression produced by the compiler is two-valued, so
//     !, && and || compose classically and a comparison on an absent
//     property is plainly false (or true, for !=) on both paths.
//   - Anything that could fail at run time (regular expressions, timestamp
//     and duration literals, unknown type, option or relation names, invalid
//     UUID literals compared to id columns) is validated or folded at compile
//     time so that evaluation never raises. Should a run-time error still
//     occur in-process, Program.Eval reports the block as not matching.
//   - text.contains is case-insensitive on both paths (ILIKE in SQL), as the
//     specification requires; startsWith, endsWith and matches are
//     case-sensitive. matches uses RE2 syntax in-process and POSIX ARE in
//     PostgreSQL; the compiler validates the pattern with RE2 and caps it at
//     512 characters.
//
// # SQL shape
//
// Every literal, including the caller DID and the query timestamp, is a bind
// parameter; no user input is ever concatenated into SQL. Property predicates
// compile to EXISTS / NOT EXISTS semi-joins over block_properties, edge
// predicates to EXISTS over block_edges, the tree functions to ltree
// operators over wbs_path and text search to the tsv column, so that every
// generated shape is served by the indexes of the specification. Pagination
// is keyset only: the cursor carries the sort keys and the id of the last row
// and expires with the schema version.
package query
