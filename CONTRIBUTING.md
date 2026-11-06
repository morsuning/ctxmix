# Contributing

Bug reports, test cases and pull requests are welcome.

## The one rule: do not change the bytes by accident

The compressed output is part of the format. `TestGolden*` pins the output of
fixed inputs, and `testdata/*.cmx` are complete streams. If a change makes one of
them fail, you have changed the format. That is allowed only deliberately:

1. bump the version byte in `frameMagic` (writer and reader),
2. update `FORMAT.md` and `CHANGELOG.md`,
3. regenerate the vectors (`go test -run Golden -update`, then update the digests
   in the tests).

Refactors, speed-ups and API additions must leave every digest unchanged.

Model code must stay integer-only. No floating point, no `unsafe`, no map
iteration order, no dependence on pointer values or on the CPU: the output has to be
bit-identical on every platform. CI runs the vectors on amd64, arm64, 386 and
s390x (big-endian).

## Before sending a change

```
gofmt -l .            # must print nothing
go vet ./...
go test -count=1 ./...                     # about a minute
go test -race -short -run 'Concurrency|Reader|Writer|Context|Close' .
go test -run '^$' -fuzz FuzzDecode$ -fuzztime 30s .
```

Add a test for every behaviour you change. A decoder fix should come with the
input that triggered it (a fuzz reproducer goes into `testdata/fuzz/<Name>/`).
The module has no dependencies and should stay that way.
