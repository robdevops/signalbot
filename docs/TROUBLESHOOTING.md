# Troubleshooting

**The bot answers only commands, and ignores mentions and DMs.** It has no model: the startup log warns "no model: set MODEL_API_KEY (or MODEL_API_URL for a provider that needs no key); until then only commands are answered". Set `MODEL_API_KEY`, or `MODEL_API_URL` for a provider that needs no key. `OPENROUTER_API_KEY` is now `MODEL_API_KEY`, and the old name is ignored, with a warning at startup.

**"signal-cli connect failed: … connection refused".** signal-cli isn't listening where the bot looks: by default `127.0.0.1:7583`, so run it with `daemon --tcp 127.0.0.1:7583` (as [`examples/signal-cli.service`](../examples/signal-cli.service) does), or set `SIGNALCLI_SOCKET` to where it listens. Under Docker, the container needs `network_mode: host` (as in `compose.yaml`) to reach the host's `127.0.0.1`.

**"Permission denied" on the Unix socket.** See [Unix socket](SOCKET.md#permission-denied-on-the-socket).

**The bot exits at startup saying it can't write its state directory.** Under systemd, the unit needs `StateDirectory=signalbot` in `[Service]` (a copy of the unit without it runs the bot in `/`, which it can't write). Under Docker, the host directory mounted at `/data` must be owned by user 65532: `sudo chown 65532 /var/lib/private/signalbot`.

---

[Back to README.md](../README.md)
