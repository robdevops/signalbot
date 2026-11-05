# Scheduling

In a DM with the bot, a sender in `SIGNAL_DM_USER_IDS` can schedule anything they could post themselves:

```
/schedule "<message or /command>" <ISO 8601 time> [every hour|day|week|month|quarter|year] [group name]
/schedule list            your schedules, numbered
/schedule del <number>    delete one
/schedule help
```

For example `/schedule "@signalbot what's the weather today?" 2030-01-31T07:30 every day Family`. At each run the text acts as if you had just posted it, in the DM or the named group: plain text is posted by the bot as written (with @Name turned into real mentions, and Markdown such as `**bold**` shown as bold), text that @mentions the bot gets its answer, and a `/command` runs.

- The time is like `2030-01-31T09:00`, in the bot's time zone (`TZ`), or with an offset, like `2030-01-31T09:00+10:00`. A one-off in the past is refused; a repeating schedule that starts in the past first runs at its next time. A monthly schedule from the 31st runs on the last day of shorter months.
- The group name is the rest of the line, with no quotes needed. It is matched among the groups you share with the bot, ignoring capitals, emoji and extra spaces, so `family` finds "🏡 Family 🎉". No match, or more than one, is refused with your groups listed. A schedule keeps the group's ID, so it survives a rename; if you leave the group its runs are dropped until you delete it.
- `/timer`, `/reminder` and `/job` work the same as `/schedule`.
- When the model can call tools, it is told these four commands, so asking it for a reminder, or mistyping one as `/time`, gets you pointed to them.
- Straight and curly quotes both work. A schedule can't run `/schedule`, `/timer`, `/reminder` or `/job`, so it can't make more schedules. The bot never answers its own posts, so a scheduled message that @mentions it gets exactly one answer.
- If the bot was down when a run was due, a repeating schedule skips it and a one-off up to an hour late still runs; an older one-off is dropped.
- Schedules are saved in `schedules.json` in `STATE_DIRECTORY` until deleted. See [PRIVACY.md](../PRIVACY.md).
