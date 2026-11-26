# Deploying llm-hops

One binary, one SQLite file, one port on loopback. Reach it through an SSH tunnel or put an authenticating
reverse proxy in front; the API has no accounts.

## A user service with systemd

```toml
# ~/.config/llm-hops/hops.toml   (llm-hops config > hops.toml prints a documented example)
listen = "127.0.0.1:11602"
db = "/home/me/.local/share/llm-hops/hops.db"
keep_days = 30
tail = ["/home/me/yllm-gateway/var/requests.jsonl"]
from_start = true
```

```ini
# ~/.config/systemd/user/llm-hops.service
[Unit]
Description=llm-hops: every hop of a request through the local LLM system
After=network.target

[Service]
ExecStart=%h/.local/bin/llm-hops serve -config %h/.config/llm-hops/hops.toml
# Environment=LLM_HOPS_TOKEN=...   when the page must be guarded; or `token` in the file (mode 600)
Restart=always
RestartSec=3
MemoryMax=512M
Nice=5

[Install]
WantedBy=default.target
```

```bash
mkdir -p ~/.local/bin ~/.local/share/llm-hops ~/.config/llm-hops
cp llm-hops-linux-amd64 ~/.local/bin/llm-hops
llm-hops config > ~/.config/llm-hops/hops.toml   # then edit it
systemctl --user daemon-reload
systemctl --user enable --now llm-hops.service
journalctl --user -u llm-hops -f
```

## A token

With `token` set (or `LLM_HOPS_TOKEN` in the unit's environment), every call but the health check needs it:
senders put `Authorization: Bearer <token>` on their posts (the CLI takes `-token` or `LLM_HOPS_TOKEN`), and a
browser opens the page once as `http://127.0.0.1:11602/#token=<token>`, which sets a cookie for thirty days.
Behind an SSH tunnel on loopback the token is optional; on anything wider it is not.

## Metrics, export, restore

`GET /metrics` is Prometheus text: requests, errors, model switches and percentiles per hop and per model
over the last five minutes, plus spans received and rejected since start.

```bash
llm-hops export -db hops.db > spans.jsonl            # every span, oldest first
llm-hops import spans.jsonl -to http://127.0.0.1:11602   # into another server; the same span never doubles
```

Spans older than `keep_days` are rolled up into one row per day and model (requests, errors, tokens, the
day's p50 and p95) and dropped, every ten minutes. The stats view then shows the rolled-up days too, marked
approximate: a percentile over rolled-up days is a request-weighted mean of the days' percentiles.

## Limits and the size of the file

| Setting | Default | What it does |
|---|---|---|
| `max_spans_per_trace` (`-max-spans-per-trace`) | 5000 | spans one trace may hold. Further spans of that trace are dropped: the sender's answer carries `dropped` and a `note`, and `llm_hops_spans_dropped_total` counts them. A span sent again still replaces its earlier self. `0` removes the cap; `/stats` and `/flow` then take about 0.3 s for a trace of 20 000 spans |
| `max_db_mb` (`-max-db-mb`) | 0 (off) | when the data is larger, whole days are rolled up and dropped, oldest first, before `keep_days` would. The current day is never touched; a warning is logged when it alone is larger |

Fixed limits: an id (`trace_id`, `span_id`, `parent_id`) is at most 64 bytes without control characters, and a
longer one is rejected (up to v0.2.4 it was cut to 64, which merged traces and could not be looked up).
`start_ms` is not negative and at most a day ahead of the server's clock; a span lasts at most 24 hours. A
filter value (`q`, `model`, ...) is at most 200 bytes.

The first start after an update adds an index to the file (a few seconds for a million spans; it is logged)
and every start checks the file with `PRAGMA quick_check`, refusing a damaged one with the reason.

A file made by this version gives room back to the disk after every prune. A file made by v0.2.4 or older
cannot (SQLite fixes that when the file is made) and the server says so at the start. To change it once:

```bash
systemctl --user stop llm-hops
llm-hops vacuum -db ~/.local/share/llm-hops/hops.db   # needs free disk of about the file's size
systemctl --user start llm-hops
```

`from_start` reads the whole log on the first run (the same line never makes two traces, so a restart is
safe) and follows it from then on. Drop it if you want history to start now.

## From another machine

Run the server where the page is read, and the adapter where the log is:

```bash
llm-hops tail /path/to/requests.jsonl -to http://127.0.0.1:11602 -from-start
```

## The setup on the author's server

The gateway logs to `~/yllm-gateway/var/requests.jsonl`. llm-hops runs as the unit above on port 11602
of the loopback interface, the SSH tunnel from the laptop forwards the same port, and the page is read at
http://127.0.0.1:11602/ on the laptop. The server has 128 GB of unified memory that belongs to the models;
llm-hops uses about 20 MB of it.

## Upgrading

Replace the binary and restart the unit. The schema is created if missing and never migrated destructively.
