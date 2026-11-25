# Scheduling

In a DM with the bot, anyone allowed to DM it (`<NETWORK>_DM_USERS`) can schedule anything they could post themselves, in that DM; in a group, only those `SCHEDULE_ALLOWED_USERS` allows, by default the group's admins and mods (see [Who may use the bot](CONFIGURATION.md#who-may-use-the-bot)):

```
/schedule "<message or /command>" <time> [every hour|day|week|month|quarter|year] [group name]
/schedule list            your schedules, numbered (or ls)
/schedule del <number>    delete one (or delete, remove, cancel, rm)
/schedule help
```

For example `/schedule "@signalbot what's the weather today?" 2030-01-31 07:30 every day Family`. At each run the text acts as if you had just posted it, in the DM or the named group: plain text is posted by the bot as written (with @Name turned into real mentions, and Markdown such as `**bold**` shown as bold), text that @mentions the bot gets its answer, and a `/command` runs.

- The time is a date and a time, like `2030-01-31 09:00` or `2030-01-31T09:00` (ISO 8601), in the bot's time zone (`TZ`), or with an offset, like `2030-01-31 09:00+10:00`. `/schedule help` and every time error show the current time this way, ready to copy and edit. A date with no time is refused, asking for one. A one-off in the past is refused; a repeating schedule that starts in the past first runs at its next time. A monthly schedule from the 31st runs on the last day of shorter months.
- The group name is the rest of the line, with no quotes needed. It is matched among the groups you share with the bot, ignoring capitals, emoji and extra spaces, so `family` finds "🏡 Family 🎉". No match, or more than one, is refused with your groups listed. A schedule keeps the group's ID, so it survives a rename; if you leave the group, or may no longer schedule posts there (`SCHEDULE_ALLOWED_USERS`, such as no longer an admin or mod), its runs are skipped until you can again or delete it, and `/schedule list` marks it; the same goes for a DM schedule once you may no longer DM the bot. On Discord, "your groups" are the channels you may post in, and a run is also dropped while you can't (not allowed to send there, or timed out); on Slack, the channels you are in.
- `/timer` and `/reminder` work the same as `/schedule`. Anything after one that isn't quoted, holds no time and isn't a subcommand is named back with what to try, such as `I don't know "/timer 10m". Try /schedule list, /schedule del <number>, or /schedule help.`
- When the model can call tools, it is told these three commands, so asking it for a reminder, or mistyping one as `/time`, gets you pointed to them.
- Straight and curly quotes both work. A schedule can't run `/schedule`, `/timer` or `/reminder`, so it can't make more schedules. The bot never answers its own posts, so a scheduled message that @mentions it gets exactly one answer.
- If the bot was down when a run was due, a repeating schedule skips it and a one-off up to an hour late still runs; an older one-off is dropped, and the bot tells you by DM: `I was offline at Sat 31 Jan 2030 09:00 AEDT, so your reminder "…" didn't post.`
- Schedules are saved in `schedules.json` in `STATE_DIRECTORY` until deleted. See [PRIVACY.md](../PRIVACY.md).

---

[Back to README.md](../README.md)
