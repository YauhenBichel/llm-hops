# Contributing

Issues and pull requests are welcome. The most useful contributions, in order:

1. **An adapter for a log you have.** `internal/adapters/yllmgateway` is the template: a function from one
   line to its spans, a test with three real lines, and the follower. Ollama's own log, llama.cpp's server,
   vLLM, LiteLLM and Open WebUI all write something that could become traces.
2. **A report from a different system.** A screenshot of the flow view of your setup, and what the page got
   wrong or did not show.
3. **The page.** It is one HTML file, one script and one stylesheet, no build step. Keep it that way: every
   piece of server text goes through `textContent`, never `innerHTML`; colours come from the CSS variables so
   both themes stay right; every chart keeps its hover.

Before a pull request:

```bash
make lint test        # go vet, gofmt, go test -race
make sdk-test         # ruff, mypy --strict, pytest for the Python SDK
```

Keep prompts and completions out of everything: spans, tests, screenshots.
