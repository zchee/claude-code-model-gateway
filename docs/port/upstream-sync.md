# Upstream sync ledger

This ledger tracks commits on the `dev` branch of the
[zchee/eigenwise-toolshed fork](https://github.com/zchee/eigenwise-toolshed)
that touch `plugins/model-gateway` after the pin
`a8501f25175572071a0d085d34346dcf67e49b23`. They are applied together in a
single sync before cutover, not ported one by one. Each commit is `pending`,
`applied`, or `not applicable`.

No commits after the pin as of 2026-11-26 JST.

| Source commit | Date | Subject | Files touched in the plugin | State | Go commit |
|---|---|---|---|---|---|
