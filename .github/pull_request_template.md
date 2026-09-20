## Summary

What this changes and why. If it changes routing behaviour, say what happens differently.

## Linked issue

Closes #

<!-- Non-trivial changes should have an issue first. See CONTRIBUTING.md. -->

## Checklist

- [ ] `gofmt -l .` is empty
- [ ] `go vet ./...` is clean
- [ ] `go test -race -count=1 ./...` passes
- [ ] Tests added or updated for this change, with no wall-clock sleeps
- [ ] README updated if behaviour or configuration changed
- [ ] No breaking change to existing config keys or to the `/status` keys `backends`,
      `totals` and `config`

<!-- If a box cannot be ticked, say why here rather than removing it. -->
