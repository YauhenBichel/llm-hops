# Privacy: what llm-hops stores, and the rule for senders

llm-hops stores spans. A span has a service, a name, two timestamps, a status and a flat map of attributes.
The server keeps whatever attributes it is sent (strings are cut at 512 characters, nested values are
dropped), and the page shows them all in the trace detail. So the rule lives with the sender:

**Never put prompt text, completion text, keys, tokens or personal data in an attribute.**

What belongs there: the client's name, the model, the role, the wire (openai, anthropic, embeddings), the
backend, the status code, durations, token *counts*, tokens per second, an error kind, the rule that made a
routing decision, whether the cache was hit or lost.

The yllm-gateway adapter reads a request log that contains no prompt or completion text by design, so
nothing of that kind can reach llm-hops from it. The Python SDK's middleware records method, path, client
address and status code; nothing from the body.

The page makes no external requests: no fonts, no scripts, no analytics. Everything it draws comes from the
server it is served by. The server listens on loopback unless told otherwise, and has no accounts: put it
behind an SSH tunnel or an authenticating proxy before exposing it.

Traces are dropped after `-keep-days` (14 by default). Delete the SQLite file to forget everything.
