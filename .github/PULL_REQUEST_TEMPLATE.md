<!--
Feature branches target `develop`. Only the release PR targets `main`.
See CONTRIBUTING.md for the branch model.
-->

## What this changes

<!-- One or two sentences. What behaviour is different after this merge? -->

## Why

<!-- The problem this solves. Link the issue if there is one. -->

Closes #

## Checklist

- [ ] `go test ./...` passes.
- [ ] `./test/e2e/run.sh` passes. **This is the gate that counts** — the
      compiled unit suite cannot catch Yaegi/compiler divergence.
- [ ] `gofmt -l .` reports nothing outside `vendor/`.
- [ ] `go mod tidy && go mod vendor` produces no diff.
- [ ] `CHANGELOG.md` has an entry under `## [Unreleased]`.
- [ ] Documentation is updated if the configuration surface changed.

## Yaegi risk

<!--
Delete this section if the change does not touch provider.go.

Otherwise: does this change add a closure over a loop variable, a
`select` send into the provider channel, a goroutine, generics, or
reflection? Each of these has behaved differently under Yaegi than under
the compiler. Say which apply and how the E2E suite covers them.
-->

## Configuration compatibility

<!--
Delete if not applicable. Does an existing config keep working unchanged?
If not, say exactly what an operator must change, and confirm the version
bump reflects it.
-->
