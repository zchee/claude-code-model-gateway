# Port plan

### 3.10 Differences from the source

| # | slug | Source behavior | Port | Reason | Evidence at the pin | Required Go replacement test | Replacement tests (phase) |
|---|---|---|---|---|---|---|---|
| — | new-form | Old behavior | New behavior | Compatibility | Source test | Human explanation with `unrelated/pkg.TestNotAnObligation` and `old\|new`. | `internal/gate.TestReplacement` (phase-4)<br>`internal/gate.TestPackagedReplacement` (phase-5) |
