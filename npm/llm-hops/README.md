# llm-hops

See every hop of a request through your local LLM system. One Go binary: a server with a web UI, and a
Model Context Protocol server over stdio so an assistant can ask what the system did.

This package installs that binary from npm, for macOS and Linux on arm64 and x64. The source, the
documentation and the other ways to install it (Homebrew, `go install`, release binaries) are at
https://github.com/YauhenBichel/llm-hops.

```bash
npx llm-hops serve -demo 300 -live          # then open http://127.0.0.1:11602/
npm install -g llm-hops                     # or keep it: llm-hops version
```

As an MCP server, against a running llm-hops server:

```bash
claude mcp add llm-hops -- npx -y llm-hops mcp -to http://127.0.0.1:11602
```

```json
{ "mcpServers": { "llm-hops": { "command": "npx", "args": ["-y", "llm-hops", "mcp", "-to", "http://127.0.0.1:11602"] } } }
```

## How it is packaged

`llm-hops` is a small launcher. The binary is in one of four packages, `llm-hops-darwin-arm64`,
`llm-hops-darwin-x64`, `llm-hops-linux-arm64` and `llm-hops-linux-x64`, which npm picks by `os` and `cpu` as
an optional dependency. No install script runs and nothing is downloaded outside npm, so it works with
`--ignore-scripts` and behind a registry mirror. The binaries are the ones on the GitHub release of the
same version.

If the launcher says the platform package is not installed, the install skipped optional dependencies
(`--omit=optional`). Install again without that flag, or set `LLM_HOPS_BINARY` to a binary you built.
