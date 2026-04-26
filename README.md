# yadcc-go

`yadcc-go` is a Go port of `yadcc`, developed as a sibling project next to the original C++ repository.

Current status: project skeleton and phase 0 migration groundwork.

## Layout

```text
cmd/
  yadcc/             compiler wrapper entry
  yadcc-daemon/      local daemon and remote worker entry
  yadcc-scheduler/   scheduler entry
  yadcc-cache/       cache service entry
internal/
  client/            wrapper behavior
  locald/            local daemon HTTP API
  remoted/           remote worker placeholder
  scheduler/         scheduler placeholder
  cache/             cache service placeholder
  platform/          platform abstraction
  protocol/          wire-format helpers
docs/
  migration-progress.md
```

## Current Limitation

The distributed compilation path is not implemented yet. The current implementation contains the project skeleton, command entries, placeholder services, and protocol helpers.

## Development

```bash
make fmt
make test
```
