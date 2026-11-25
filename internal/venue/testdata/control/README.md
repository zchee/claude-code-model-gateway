# Venue negative control

Build on the approved, attested Linux venue from the module root. Use a fresh private temporary directory for `out`:

```sh
out=$(mktemp -d)
```

Build the default fixture:

```sh
go test -c -o "$out/default.test" ./internal/venue/testdata/control
```

Build the process-tag fixture separately:

```sh
go test -c -tags process -o "$out/process.test" ./internal/venue/testdata/control
```

The remote control harness invokes each binary directly with `MODEL_GATEWAY_VENUE_SENTINEL` set to a fresh, nonexistent path in that directory. The only test writes `default\n` or `process\n`, respectively. A missing attestation must instead produce exactly `not a test venue: attestation missing` and a newline on stderr, exit 97, and leave the sentinel absent. Never build or run these controls on the developer machine.
