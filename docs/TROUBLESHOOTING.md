# Troubleshooting

**The bot answers only commands, and ignores mentions and DMs.** It has no model: the startup log says "no model: only commands are answered" (a notice, which `journalctl -p warning` hides). Set `MODEL_API_KEY`, or `MODEL_API_URL` for a provider that needs no key. `OPENROUTER_API_KEY` is now `MODEL_API_KEY`, and the old name is ignored.

**Permission denied on the socket.** Whatever connects to `/run/signal-cli/socket` must be in the `signal-cli` Unix group, and the socket must be group-writable (`ls -l /run/signal-cli/socket` should show `srwxrwx---` and the group `signal-cli`). Common causes:

- A service started before the group existed. Restart it: `sudo systemctl restart signal-cli signalbot`.
- You added yourself to the group (`sudo usermod -aG signal-cli "$USER"`) to test the socket with `socat`. Your open shell keeps its old groups until you log out and back in, or run `newgrp signal-cli`.
- You run signal-cli from a shell instead of the systemd unit. It then uses your own group and umask, so the socket is `0755` and the group cannot write to it. Start it with the right group and umask, once `/run/signal-cli` exists with that group:

  ```
  # create socket directory:
  sudo install -d -o "$USER" -g signal-cli -m 0750 /run/signal-cli

  # run signal-cli
  sudo -u "$USER" -g signal-cli sh -c 'umask 007 && signal-cli -o json daemon --socket /run/signal-cli/socket --no-receive-stdout'
  ```

  On Linux, `sg signal-cli -c '…'` does the same without `sudo` if you are in the group. `doas` cannot set a group, so on OpenBSD use `su` or the system's `rc.d` service.
