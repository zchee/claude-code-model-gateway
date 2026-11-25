# Tool-schema fixtures

`node-inputs.json` is an array of cases with unique `name` strings. Look up each result by name in `node-golden.json` under `cases`. The fixtures record the public `adaptCodexToolSchemas` behavior of the pinned JavaScript source; no Go validator is needed to read them.

Each case supplies exactly one input:

- `schema`: pass `[{"name":"artifact_tool","input_schema":<schema>}]` to the adapter and observe the returned tool's `input_schema`.
- `tools`: pass the value as given and observe the entire return value, even when it is not an array.

On success, `wire` is the exact UTF-8 text from compact `JSON.stringify` of the observed value, without a trailing newline. Compare the decoded string's bytes, not the outer fixture's JSON spelling. Preserve property order: JavaScript enumerates array-index keys first in ascending numeric order, then other keys in insertion order. This also determines which refusal wins when multiple schema members would be refused.

`edits` lists changed strings in traversal order. Each `pointer` is an absolute RFC 6901 pointer into a document of the form `{"tools": tools}`. It includes the tool index and `input_schema`, even for a `schema` case; `replacement` is the full adapted pattern. `same` records JavaScript `===` between the input and the observed result. `sameTools`, when present for an explicit tool array, compares each output element with its original. These distinguish a no-op from an equal copy. `selfCycle: true` is allowed only on an object schema without a `self` member and without `instances`. To construct it, set `schema.self = schema`. The result's `selfCycle` says whether that link still points to the result itself. A cyclic value has no JSON `wire`.

On refusal, `error` holds the adapter's `reasonCode`, escaped `pointer`, and `toolName`. There is no returned schema or edit list. The generator checks that the original input survives both success and refusal, including its serialized property order for acyclic cases.

Optional `instances` on a `schema` case request independent Ajv Draft 2020-12 evidence (`strict: false`, `unicodeRegExp: true`). `before` and `after` are boolean arrays aligned with the instances for the original and adapted schemas, respectively; an empty array asks only whether the schema compiles. `expectInvalidRewrittenPattern: true` requires `instances` and an original schema that compiles. Only for that case does `after: { "compiled": false }` record Ajv's invalid-regular-expression diagnostic for the rewrite; a successful compile or a different failure stops generation. The adapter can return such a pattern because it checks the original pattern before removing property escapes. A refused case has no `after`. These fields document the validator's behavior, not a validation step the Go adapter should perform.

From the repository root, capture the venue output in a temporary file. This requires a warm toolchain cache: if `hack/remote` installs or refreshes Go or Node, it also writes checksum lines to stdout. In that case, repeat the capture with the cache warm; do not strip lines from the fixture.

```bash
out=$(mktemp) && VENUE_HOST=debian-13-trixie.gaudiy-platform ./hack/remote run toolschema-goldens >| "$out"
```

After a successful capture, compare the complete file byte for byte in the same shell. No output and exit status 0 mean the files match.

```bash
cmp "$out" internal/toolschema/testdata/node-golden.json
```
