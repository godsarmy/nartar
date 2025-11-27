# nartar

CLI to convert Nix NAR archives to tar files and back using the `go-nix` NAR reader/writer. Timestamps are normalized to the Unix epoch so tar outputs match the typical `/nix/store` default (`31 Dec 1969` depending on timezone).

## Usage

```
go run ./cmd/nartar nar2tar -i input.nar -o output.tar
go run ./cmd/nartar tar2nar -i input.tar -o output.nar
```

Use `-` for stdin/stdout.

Build a binary:

```
go build -o nartar ./cmd/nartar
```

## Development

Run the CLI tests (none are defined yet, but this compiles the binary) with a local cache path to avoid sandbox issues:

```
GOCACHE=$(pwd)/.gocache go test ./cmd/nartar
```
