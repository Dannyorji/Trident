# ScVal → JSON mapping

The parser (`crates/indexer/src/parser/mod.rs`) decodes every Soroban event
topic and data payload from XDR into a `stellar_xdr::curr::ScVal`, then
projects it into two forms:

- `scval_to_string` — a compact scalar used for event **topics** (the
  `topics: Vec<String>` column).
- `scval_to_json` — a `serde_json::Value` used for the event **body**
  (the `data` column) and for recursive values (map/vec entries).

`ScVal` has exactly 22 variants (`stellar-xdr` 26.0.1). Both functions match
every variant explicitly with **no wildcard arm** — a future `stellar-xdr`
upgrade that adds a 23rd variant is a compile error here, not a silent
debug-format fallback (issue #209). This table is the canonical reference for
what each variant produces.

| `ScVal` variant | `scval_to_json` shape | `scval_to_string` shape |
|---|---|---|
| `Bool` | JSON boolean | `"true"` / `"false"` |
| `Void` | JSON `null` | `"void"` |
| `Error(ScError)` | JSON string, `{:?}` debug form | same |
| `U32` / `I32` | JSON number | decimal string |
| `U64` / `I64` | JSON number | decimal string |
| `Timepoint` / `Duration` | JSON string (u64 as decimal) | decimal string |
| `U128` / `I128` | JSON number if it fits in an i64/u64, else a decimal string | full-width decimal string |
| `U256` / `I256` | decimal string | decimal string |
| `Bytes` | hex string (**not** base64) | hex string |
| `String` / `Symbol` | UTF-8 string (lossy) | UTF-8 string (lossy) |
| `Vec(Some(items))` | JSON array of the recursively-converted items | `[item,item,...]` |
| `Vec(None)` | `[]` | `"Vec(None)"` |
| `Map(Some(entries))` | JSON object; each key is `scval_to_string(key)`, each value `scval_to_json(val)` | `{key:val,...}` |
| `Map(None)` | `{}` | `"Map(None)"` |
| `Address` | strkey string (`G...`/`C...`) | strkey string |
| `ContractInstance` | `{"executable": {...}, "storage": {...} \| null}` — see below | `"contract_instance(wasm:<hex>)"` or `"contract_instance(stellar_asset)"` |
| `LedgerKeyContractInstance` | `"ledger_key_contract_instance"` | `"ledger_key_contract_instance"` |
| `LedgerKeyNonce` | `{"nonce": "<decimal string>"}` | decimal string |

## `ContractInstance`

`ScContractInstance` carries which code a contract runs plus its persistent
instance storage:

```json
{
  "executable": { "type": "wasm", "wasm_hash": "<64 hex chars>" },
  "storage": { "...": "..." }
}
```

or, for a built-in Stellar Asset Contract:

```json
{
  "executable": { "type": "stellar_asset" },
  "storage": null
}
```

`storage` is `null` when the instance carries no storage map, otherwise an
object built the same way as `Map(Some(...))` above (recursively, so a
storage map can itself hold nested maps/vecs/contract instances without
truncation).

## `LedgerKeyNonce`

Carries a single `i64` nonce, used to address a ledger key rather than to
represent contract-defined data:

```json
{ "nonce": "99" }
```

The nonce is emitted as a string, consistent with every other u64/i64-valued
scalar in this mapping (`Timepoint`, `Duration`, `U64`, `I64`) — values above
2^53 do not survive a JSON number round-trip through a JavaScript consumer.

## Why no variant is "unsupported"

An earlier version of this decoder fell through to a Debug-format string plus
a `trident_indexer_unhandled_scvariant_total` metric increment for any variant
it didn't explicitly handle. That metric identified `ContractInstance`,
`LedgerKeyContractInstance`, and `LedgerKeyNonce` as the three unhandled
variants (issue #209's audit); all three now have the documented shapes
above and the metric has been removed since it can no longer fire — the match
is exhaustive.

## Test coverage

`crates/indexer/src/parser/mod.rs::tests::every_scval_variant_has_a_documented_json_shape`
is a table-driven golden test: one entry per variant, asserting the exact
JSON produced, that it round-trips through `serde_json`, and that
`scval_to_string` does not panic on the same value. A companion test
(`nested_map_inside_vec_inside_contract_instance_storage_decodes_recursively`)
covers a three-level-deep nested structure (map → vec → map) inside
`ContractInstance.storage`.

The proptest suite in the same module (`decode_scval_never_panics_on_*`,
`scval_to_json_never_panics`, `scval_to_string_never_panics`) additionally
fuzzes both functions against random/mutated XDR — see issue #219 and
[`docs/indexer/xdr-parser-fuzzing.md`](xdr-parser-fuzzing.md).
