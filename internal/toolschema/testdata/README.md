# Tool-schema fixtures

`node-inputs.json` is an array of cases with unique `name` strings. Look up each result by name in `node-golden.json` under `cases`. The fixtures record the public `adaptCodexToolSchemas` behavior of the pinned JavaScript source; no Go validator is needed to read them.

Each case supplies exactly one input:

- `schema`: pass `[{"name":"artifact_tool","input_schema":<schema>}]` to the adapter and observe the returned tool's `input_schema`.
- `tools`: pass the value as given and observe the entire return value, even when it is not an array.

On success, `wire` is the exact UTF-8 text from compact `JSON.stringify` of the observed value, without a trailing newline. Compare the decoded string's bytes, not the outer fixture's JSON spelling. Preserve property order: JavaScript enumerates array-index keys first in ascending numeric order, then other keys in insertion order. This also determines which refusal wins when multiple schema members would be refused.

`edits` lists changed strings in traversal order. Each `pointer` is an RFC 6901 path from `/tools`, including the index and `input_schema`, even for a `schema` case; `replacement` is the full adapted pattern. `same` records JavaScript `===` between the input and the observed result. `sameTools`, when present for an explicit tool array, compares each output element with its original. These distinguish a no-op from an equal copy. For `selfCycle: true`, first set `schema.self = schema`. The result's `selfCycle` says whether that link still points to the result itself. A cyclic value has no JSON `wire`.

On refusal, `error` holds the adapter's `reasonCode`, escaped `pointer`, and `toolName`. There is no returned schema or edit list. The generator checks that the original input survives both success and refusal, including its serialized property order for acyclic cases.

Optional `instances` on a `schema` case request independent Ajv Draft 2020-12 evidence (`strict: false`, `unicodeRegExp: true`). `before` and `after` are boolean arrays aligned with the instances for the original and adapted schemas, respectively; an empty array asks only whether the schema compiles. `{ "compiled": false }` records a pattern that the validator could not compile, not a refusal by the adapter. The adapter can return such a pattern because it checks the original pattern before removing property escapes. A refused case has no `after`. These fields document the validator's behavior, not a validation step the Go adapter should perform.

From the repository root, compare freshly generated output to the tracked fixture byte for byte. The generator runs only in the attested venue; npm diagnostics go to stderr.

```bash
(set -o pipefail; VENUE_HOST=debian-13-trixie.gaudiy-platform ./hack/remote run toolschema-goldens | cmp - internal/toolschema/testdata/node-golden.json)
```
