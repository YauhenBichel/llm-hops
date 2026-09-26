# Deploying llm-hops

One binary, one SQLite file, one port on loopback. Reach it through an SSH tunnel or put an authenticating
reverse proxy in front; the API has no accounts.

## A user service with systemd

```ini
# ~/.config/systemd/user/llm-hops.service
[Unit]
Description=llm-hops: every hop of a request through the local LLM system
After=network.target

[Service]
ExecStart=%h/.local/bin/llm-hops serve -db %h/.local/share/llm-hops/hops.db -listen 127.0.0.1:11602 \
  -tail %h/yllm-gateway/var/requests.jsonl -from-start -keep-days 30
Restart=always
RestartSec=3
MemoryMax=512M
Nice=5

[Install]
WantedBy=default.target
```

```bash
mkdir -p ~/.local/bin ~/.local/share/llm-hops
cp llm-hops-linux-amd64 ~/.local/bin/llm-hops
systemctl --user daemon-reload
systemctl --user enable --now llm-hops.service
journalctl --user -u llm-hops -f
```

`-from-start` reads the whole log on the first run (the same line never makes two traces, so a restart is
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
