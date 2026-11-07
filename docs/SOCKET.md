# Unix socket

By default the bot reaches signal-cli over TCP on `127.0.0.1:7583`. That is the simplest setup, but **anyone with a login or a process on the host can connect to that port and read or send messages as your Signal account** (PRIVACY RISK). signal-cli's Unix socket closes that hole: its file permissions let only a Unix group, which only the bot joins, connect. Use it on any host you share, or wherever the account should be the bot's alone.

## Set it up

1. Create the group: `sudo groupadd --system signal-cli`.
2. Run signal-cli on the socket, creating it group-writable in a directory the group can search. With [`examples/signal-cli.service`](../examples/signal-cli.service), add a drop-in (`sudo systemctl edit signal-cli`):
   ```
   [Service]
   # The group the bot joins to reach the socket.
   Group=signal-cli
   # Makes the socket group-writable (0770). The default umask gives 0755, which
   # locks the group out.
   UMask=0007
   # /run/signal-cli, searchable by the group.
   RuntimeDirectory=signal-cli
   RuntimeDirectoryMode=0750
   ExecStart=
   ExecStart=/usr/local/bin/signal-cli -o json daemon --socket /run/signal-cli/socket --no-receive-stdout
   ```
   Then `sudo systemctl restart signal-cli`, and check with `ls -l /run/signal-cli/socket`: it should show `srwxrwx---` and the group `signal-cli`.
3. Keep signal-cli's data directory at mode `0700` (`chmod 700 ~/.local/share/signal-cli`). `UMask=0007` makes new files in it group-accessible, and the bot is in that group, so the directory's own mode is what keeps the bot from your account keys. Don't run the bot as signal-cli's user.
4. Set `SIGNALCLI_SOCKET=/run/signal-cli/socket` in `/etc/signalbot.env`.
5. Let the bot join the group:
   - **systemd:** uncomment `SupplementaryGroups=signal-cli` in `signalbot.service` (or add it in a drop-in, `sudo systemctl edit signalbot`), then `sudo systemctl daemon-reload` and `sudo systemctl restart signalbot`.
   - **Docker:** in the `docker run` command, replace `--network=host` with `-v /run/signal-cli:/run/signal-cli --group-add "$(getent group signal-cli | cut -d: -f3)"`. The container then mounts only the socket's directory, so the data directory isn't there to read, and keeps its own network.

## How access works

The socket's permissions decide who may connect, and the bot is let in through a Unix group, not a user id, so a transient `DynamicUser` works:

- `signalbot.service` runs with `DynamicUser=yes` and `SupplementaryGroups=signal-cli`, so its throwaway user is in the group.
- Docker's `--group-add` puts the container's user in the same group.
- signal-cli creates the socket group-writable (`0770`, from `UMask=0007`) in a directory the group can search (`0750`).

Everyone outside the group gets "Permission denied". The group is the only access control: anyone in it can drive the Signal account, so keep it to the bot.

The bot's keys stay out of reach in two ways. It runs as a different user from signal-cli, and the data directory is mode `0700`, so it can't be searched. That mode matters: with a `0755` directory the bot, in signal-cli's group, could read the group-accessible files `UMask=0007` creates (tested). Also, `ProtectHome=yes` hides `/home`, where signal-cli keeps its data by default (`~/.local/share/signal-cli`). Data kept elsewhere, such as `/var/lib/signal-cli`, relies on the directory mode alone; add it to `InaccessiblePaths=` in the unit to hide it outright.

## Permission denied on the socket

Whatever connects to `/run/signal-cli/socket` must be in the `signal-cli` group, and the socket must be group-writable. Common causes:

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

---

[Back to README.md](../README.md)
