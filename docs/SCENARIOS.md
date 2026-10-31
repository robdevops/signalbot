# Old tests and the scenarios that cover them

Step 1 of [RECODE-v3.md](RECODE-v3.md) pins the bot's behaviour at `e40dc5f` with
behaviour scenarios that touch only the edges: messages in on a fake signal-cli
socket, then what the bot sends back, what a fake OpenRouter receives, and the log.

- `scenario_test.go`: the scenarios and the two golden prompt tests. They use only the harness.
- `scenario_fakes_test.go`: the fakes and the harness API (fake signal-cli, fake OpenRouter, two fake MCP servers plus an optional third whose tools fail, the captured log). It names nothing in the bot's code.
- `scenario_start_test.go`: `startBot`, which wires one version of the bot to the fakes. It is the only file that names the bot's code, and the only one rewritten for the new core.
- `testdata/prompt_full.txt` and `testdata/prompt_nofeatures.txt`: the golden system prompts. `go test -run Golden -update` rewrites them.

Every scenario also checks, when it ends, that no message text, model reply, tool argument or tool result it used reached any log line. Each text is looked for as a log line would hold it (newlines escaped), line by line, and by the first 24 runes of each line, so a leak that is cut short or quoted is caught too; pieces under 8 runes are skipped, and of a command only the words after its name are checked. The log is captured at `LOG_LEVEL=debug`, with journald priorities.

The table maps every `Test*` function in the 19 old test files to what covers it. "Unit test, moves with its code" means a test of a pure helper (drawing, splitting, parsing, config) or of code that is only reachable from inside, which moves into the new package with that code.

| Old test | File | Covered by |
|---|---|---|
| TestDecide | bot_test.go | TestScenarioGroupMentionIsAnswered, TestScenarioReplyToBotIsAnswered, TestScenarioAllowedDMIsAnswered, TestScenarioDMFromStrangerIsDroppedAndLoggedByID, TestScenarioUnaddressedMessagesAreNeverLogged, TestScenarioGroupSenderWithoutName (no name prefix), TestScenarioAtMostFourImages |
| TestEndToEnd | bot_test.go | TestScenarioGroupMentionIsAnswered, TestScenarioTypingBeforeReply (typing before the reply, none after it), TestScenarioDMFromStrangerIsDroppedAndLoggedByID |
| TestMessageLogLine | bot_test.go | Unit test, moves with its code (the "mention from Alice(id)" line becomes "received group chat", approved in section 10); the escaping is TestScenarioDisplayNameIsMadeSafe |
| TestReplyLogLine | bot_test.go | TestScenarioModelRequestSettings (cost logged); the line's format is a unit test, moves with its code |
| TestLogTimestampOnlyWhenInteractive | bot_test.go | Unit test, moves with its code |
| TestSystemPromptIntroduction | bot_test.go | TestScenarioGoldenPromptFull, TestScenarioGoldenPromptNoFeatures, TestScenarioBotWithoutProfileNameIsSignalbot |
| TestSystemPromptIntroOverride | bot_test.go | TestScenarioIntroPlaceholders |
| TestSystemPromptNamesTools | bot_test.go | TestScenarioGoldenPromptFull |
| TestRenderChart | chart_test.go | Unit test, moves with its code |
| TestParseChartChangeMode | chart_test.go | Unit test, moves with its code |
| TestParseChartErrors | chart_test.go | Unit test, moves with its code |
| TestNiceTicks | chart_test.go | Unit test, moves with its code |
| TestDateTicks | chart_test.go | Unit test, moves with its code |
| TestSpreadLabels | chart_test.go | Unit test, moves with its code |
| TestChartTwoAxes | chart_test.go | Unit test, moves with its code |
| TestChartInvertedAxis | chart_test.go | Unit test, moves with its code |
| TestRenderTwoAxisSample | chart_test.go | Unit test, moves with its code |
| TestAreaStyle | chart_test.go | Unit test, moves with its code |
| TestChartArgErrors | chart_test.go | TestScenarioToolFailuresGoBackToTheModel (bad arguments reach the model as "error: ..."); the messages are a unit test, moves with its code |
| TestChartArgsDoubleEncoded | chart_test.go | Unit test, moves with its code |
| TestChartCaption | chart_test.go | TestScenarioChartReply (a title alone); the rest is a unit test, moves with its code |
| TestChatCommandsConfig | chatcommands_test.go | TestScenarioChatCommandForcesTool, TestScenarioChatCommandRequiresArg (good commands); bad and taken names are a unit test, moves with its code |
| TestParseCommand | chatcommands_test.go | TestScenarioChatCommandRequiresArg, TestScenarioUnknownCommandIsAMessage, TestScenarioExampleWithoutModel ("/EXAMPLE"); the rest is a unit test, moves with its code |
| TestChatCommandForcesTool | chatcommands_test.go | TestScenarioChatCommandForcesTool |
| TestChatCommandRequiresArg | chatcommands_test.go | TestScenarioChatCommandRequiresArg |
| TestContactsReloadOnConnectAndGroupChange | contacts_test.go | TestScenarioContactsReloadWhenAGroupChanges, TestScenarioUnknownMentionReloadsContacts, TestScenarioUnaddressedMessagesAreNeverLogged (no load for chatter); "a burst of changes costs one load" is not pinned (see below) |
| TestContactsOverADayOldReloadOnAMention | contacts_test.go | Not pinned: needs the clock moved by a day; unit test, moves with its code |
| TestGroupChangeForgetsThatChatsShownNames | contacts_test.go | TestScenarioGroupChangeForgetsMentionNames |
| TestParseDonut | donut_test.go | Unit test, moves with its code |
| TestParseDonutErrors | donut_test.go | Unit test, moves with its code |
| TestWrapLabel | donut_test.go | Unit test, moves with its code |
| TestRenderDonut | donut_test.go | Unit test, moves with its code |
| TestDonutToolInLoop | donut_test.go | TestScenarioPieChartReply (offered on "pie" and "donut", caption, attachment and history note; it finds the tool as make_donut_chart or make_pie_chart, as the rename is approved); the summary text is a unit test, moves with its code |
| TestDonutTotal | donut_test.go | Unit test, moves with its code |
| TestForgedQuoteIsEscaped | fixes_test.go | TestScenarioQuoteIsEscapedAndCapped |
| TestDisplayNameCannotInjectTurns | fixes_test.go | TestScenarioDisplayNameIsMadeSafe |
| TestMentionNamesCannotInjectTurns | fixes_test.go | TestScenarioContactNameIsMadeSafe |
| TestMentionHijackByRenameIsRefused | fixes_test.go | TestScenarioClaimedNameIsNotLinked (both orders), TestScenarioOwnNameTwiceIsLinked |
| TestUnauthorisedDMDoesNoWork | fixes_test.go | TestScenarioDMFromStrangerIsDroppedAndLoggedByID |
| TestHandleBoundsGoroutines | fixes_test.go | TestScenarioAtCapacityDrops |
| TestSplitMessageIsLinear | fixes_test.go | Unit test, moves with its code |
| TestReplyIsCappedToAFewMessages | fixes_test.go | TestScenarioHugeReplyIsCapped |
| TestChatsAreCapped | fixes_test.go | Unit test, moves with its code (memory caps are not visible at the edges) |
| TestNameCacheIsBounded | fixes_test.go | Unit test, moves with its code |
| TestOversizedFrameDropsConnectionAndReconnects | fixes_test.go | TestScenarioOversizedFrameReconnects |
| TestStripMentionsHugeOffsetsDoNotPanic | fixes_test.go | Unit test, moves with its code |
| TestOutOfOrderMentionsSpliceCorrectly | fixes_test.go | Unit test, moves with its code |
| TestSocketNetwork | fixes_test.go | Unit test, moves with its code |
| TestConfigParsing | fixes_test.go | TestScenarioDMAllowListSeparators, TestScenarioWebSearchOff; the rest is a unit test, moves with its code |
| TestClampKeepsRunesWhole | fixes_test.go | Unit test, moves with its code |
| TestSelfNameRetryIsNotBurnedByThrottle | fixes_test.go | Unit test, moves with its code (needs the contact list to change between two throttled loads) |
| TestShutdownWaitsForRepliesInFlight | fixes_test.go | TestScenarioShutdownDrainsReplies; the timeout is a unit test, moves with its code |
| TestDuplicateMessagesIgnoredWithinWindow | fixes_test.go | TestScenarioDuplicateIsDropped, TestScenarioDifferentPhotosAreNotDuplicates; "after the window" needs the clock and is a unit test, moves with its code |
| TestStripSelfTags | fixes_test.go | TestScenarioReplyMentions; the other cases are a unit test, moves with its code |
| TestSystemPromptLayering | fixes_test.go | TestScenarioSystemPromptLayering |
| TestLogLineOmitsBody | fixes_test.go | Every scenario (no message text in any log line) |
| TestCloseStopsNewReplies | fixes_test.go | TestScenarioShutdownDrainsReplies |
| TestUnaddressedGroupMessageTeachesNothing | fixes_test.go | TestScenarioUnaddressedMessagesAreNeverLogged |
| TestShutdownDrainsReplyBeforeClosingSocket | fixes_test.go | TestScenarioShutdownDrainsReplies |
| TestReconnectsDoNotLeakGoroutines | fixes_test.go | Unit test, moves with its code |
| TestClientRequiresTLS13 | fixes_test.go | Unit test, moves with its code (the fake OpenRouter is plain HTTP) |
| TestUnaddressedMessagesAreNeverLogged | fixes_test.go | TestScenarioUnaddressedMessagesAreNeverLogged |
| TestDMFromUnlistedSenderIsLoggedByIDOnly | fixes_test.go | TestScenarioDMFromStrangerIsDroppedAndLoggedByID |
| TestOnlyUnlistedDMsGetTheIgnoredLine | fixes_test.go | TestScenarioUnaddressedMessagesAreNeverLogged |
| TestDisappearingMessagesAreErasedFromHistory | fixes_test.go | TestScenarioDisappearingMessagesLeaveHistory |
| TestHistoryExpiry | fixes_test.go | TestScenarioDisappearingMessagesLeaveHistory (the timer); the 24-hour cap needs the clock, unit test, moves with its code |
| TestGroupChatterIsContext | groupcontext_test.go | TestScenarioGroupChatterIsContext (with the bot's own group message left out), TestScenarioGroupSenderWithoutName ("someone") |
| TestGroupChatterNotes | groupcontext_test.go | TestScenarioGroupChatterNotesImagesWithoutFetching |
| TestGroupContextOff | groupcontext_test.go | TestScenarioGroupContextOff |
| TestOneEraserPerChat | groupcontext_test.go | Unit test, moves with its code (timers are not visible at the edges) |
| TestGroupContextConfig | groupcontext_test.go | TestScenarioGroupChatterIsContext (default on), TestScenarioGroupContextOff |
| TestPhotoTriggers | images_test.go | TestScenarioPhotoIsShownToModel, TestScenarioUnaddressedMessagesAreNeverLogged (voice note), TestScenarioMessageWithNothingUsableIsIgnored (too big), TestScenarioGroupChatterNotesImagesWithoutFetching (group photo) |
| TestQuotedPhotoIsSeen | images_test.go | TestScenarioImagesAreFetchedFromTheirChat |
| TestPhotoShownToModel | images_test.go | TestScenarioPhotoIsShownToModel |
| TestQuotedPhotoFetchedByGroup | images_test.go | TestScenarioImagesAreFetchedFromTheirChat |
| TestPhotoWithoutVision | images_test.go | TestScenarioPhotoWithoutVision |
| TestChatMsgWithImagesJSON | images_test.go | TestScenarioPhotoIsShownToModel (the fake reads the content parts); the exact JSON is a unit test, moves with its code |
| TestCapabilities | images_test.go | TestScenarioPhotoIsShownToModel, TestScenarioModelWithoutTools, TestScenarioModelListUnavailable, TestScenarioModelNotListed |
| TestDMQuotedPhoto | images_test.go | TestScenarioImagesAreFetchedFromTheirChat |
| TestStickerTriggers | images_test.go | TestScenarioStickerIsShownToModel, TestScenarioGroupChatterNotesImagesWithoutFetching (group sticker) |
| TestStickerShownToModel | images_test.go | TestScenarioStickerIsShownToModel, TestScenarioStickerWithoutVision |
| TestStickerLabel | images_test.go | TestScenarioStickerIsShownToModel (a known pack); unknown packs and hostile titles are a unit test, moves with its code |
| TestGroupPhotoWithMention | images_test.go | TestScenarioImagesAreFetchedFromTheirChat |
| TestStdoutIsJournal | journal_unix_test.go | Unit test, moves with its code |
| TestJournalLinesCarryTheirPriority | log_test.go | Unit test, moves with its code (the harness reads the priorities) |
| TestOtherLinesNameTheirLevel | log_test.go | Unit test, moves with its code (changes: plain lines without journald, approved) |
| TestLogLevelFilters | log_test.go | Unit test, moves with its code (`LOG_LEVEL` is dropped, approved) |
| TestJournalPrefixCannotBeForged | log_test.go | TestScenarioLoggedValuesCannotForgeLines (a stranger's id with a newline); the prefix itself is a unit test, moves with its code |
| TestLogLevelConfig | log_test.go | Unit test, moves with its code (`LOG_LEVEL` is dropped, approved) |
| TestStreamsSplitWithoutJournal | log_test.go | Unit test, moves with its code |
| TestJournalUsesStdoutOnly | log_test.go | Unit test, moves with its code |
| TestBuildVersion | main_test.go | Unit test, moves with its code |
| TestLoadMCPConfig | mcp_test.go | Unit test, moves with its code |
| TestToolModelName | mcp_test.go | Unit test, moves with its code |
| TestHTTPMCPTools | mcp_test.go | TestScenarioChatCommandForcesTool, TestScenarioToolImageIsSentWithReply (JSON replies), TestScenarioToolFailuresGoBackToTheModel (an error result and a JSON-RPC error); SSE and paging are a unit test, moves with its code |
| TestMCPToolLists | mcp_test.go | Unit test, moves with its code |
| TestHTTPMCPSessionExpiry | mcp_test.go | Unit test, moves with its code |
| TestMCPStartupErrors | mcp_test.go | Unit test, moves with its code |
| TestMCPHelperProcess | mcp_test.go | Unit test, moves with its code (a helper for TestStdioMCP) |
| TestStdioMCP | mcp_test.go | Unit test, moves with its code |
| TestMCPByServer | mcp_test.go | TestScenarioGoldenPromptFull |
| TestReadSSEMultiLine | mcp_test.go | Unit test, moves with its code |
| TestBuildMentions | mentions_test.go | TestScenarioReplyMentions, TestScenarioMentionNamesFromContacts, TestScenarioMentionOffsetsAreUTF16 (an emoji counts as two units); the other cases are a unit test, moves with its code |
| TestDecideCollectsNamesForGroupsOnly | mentions_test.go | TestScenarioDMReplyLinksNoNames, TestScenarioReplyMentions |
| TestContactDisplayName | names_test.go | Unit test, moves with its code |
| TestResolvePriority | names_test.go | TestScenarioMentionNamesFromContacts; the ranking is a unit test, moves with its code |
| TestMentionNameResolvedFromContacts | names_test.go | TestScenarioMentionNamesFromContacts, TestScenarioUnknownMentionReloadsContacts |
| TestUnnamedMentionNotAtPrefixed | names_test.go | TestScenarioUnknownMentionIsUnnamed |
| TestConfigKeyAndModel | openrouter_test.go | TestScenarioModelRequestSettings (default reasoning); required key and default model are a unit test, moves with its code |
| TestOpenRouterRequestAndReply | openrouter_test.go | TestScenarioModelRequestSettings |
| TestOpenRouterOmitsOptionsWhenOff | openrouter_test.go | TestScenarioWebSearchOff; `REASONING=none` is a unit test, moves with its code (renamed `MODEL_REASONING`, approved) |
| TestOpenRouterErrors | openrouter_test.go | TestScenarioModelError, TestScenarioModelRetry (one retry for a 5xx or 429, no more); body parsing is a unit test, moves with its code |
| TestPanicWhileAnsweringIsContained | privacy_test.go | Not pinned: a panic can't be caused from the edges; unit test, moves with its code |
| TestAPIErrorTextKeepsOnlyTheMessage | privacy_test.go | TestScenarioModelError |
| TestBadReplyBodiesAreNotEchoed | privacy_test.go | Unit test, moves with its code |
| TestBadFrameIsNotQuoted | privacy_test.go | TestScenarioBadFrameIsNotQuoted |
| TestQuotedMessageOfSomeoneElseIsPassedAsContext | quote_test.go | TestScenarioQuoteOfSomeoneElse |
| TestQuotedAuthorWithoutAKnownNameIsSomeone | quote_test.go | TestScenarioQuoteOfUnknownAuthorIsSomeone |
| TestQuoteOfSomeoneElseIsEscaped | quote_test.go | TestScenarioQuoteIsEscapedAndCapped |
| TestReplyToSomeoneElseWithoutTaggingIsStillIgnored | quote_test.go | TestScenarioUnaddressedMessagesAreNeverLogged |
| TestUnmatchedResponsesAreLogged | signal_test.go | TestScenarioFailedSendIsLogged; "not delivered" results are a unit test, moves with its code |
| TestToolCallLoop | toolloop_test.go | TestScenarioChatCommandForcesTool (result sent back after the replayed call with a matching id, cost summed, nothing logged), TestScenarioToolImageIsSentWithReply, TestScenarioToolFailuresGoBackToTheModel |
| TestToolCallRoundCap | toolloop_test.go | TestScenarioToolLoopIsCapped |
| TestExample | toolloop_test.go | TestScenarioExampleWithoutModel |
| TestChartToolInLoop | toolloop_test.go | TestScenarioChartReply (with the replayed call and a matching id) |
| TestReplyCarriesImages | toolloop_test.go | TestScenarioChartReply, TestScenarioToolImageIsSentWithReply; "an image with no text" can't happen through OpenRouter (an empty reply is an error) |
| TestWantsChart | toolloop_test.go | TestScenarioChartToolOffering; the word list is a unit test, moves with its code |
| TestToolListOffers | toolloop_test.go | TestScenarioChartToolOffering, TestScenarioUnknownCommandIsAMessage, TestScenarioModelWithoutTools |
| TestSystemPromptTools | toolloop_test.go | TestScenarioGoldenPromptFull, TestScenarioGoldenPromptNoFeatures |
| TestChartFollowUps | toolloop_test.go | TestScenarioChartToolOffering, TestScenarioErasedHistoryEndsChartFollowUps |
| TestSentImagesNote | toolloop_test.go | TestScenarioChartReply, TestScenarioToolImageIsSentWithReply |

## Scenarios with no old test

TestScenarioHistoryAcrossTurns, TestScenarioHistoryIsPerChat, TestScenarioHistoryTurnsCap, TestScenarioLongReplyIsSplit, TestScenarioModelRetry, TestScenarioUnknownCommandIsAMessage, TestScenarioBotWithoutProfileNameIsSignalbot, TestScenarioAtMostFourImages, TestScenarioGroupSenderWithoutName and TestScenarioErasedHistoryEndsChartFollowUps pin behaviour that had no direct test before.

## Not pinned, or pinned with care

- **The 15-second contact reload limit.** `startBot` lowers it to `contactReloadGap` (50ms), so a scenario can see a reload after a group change, or after an unknown mention once it has waited out the gap. A burst of group changes costing one reload depends on that timing, so it isn't asserted.
- **Typing every 10 seconds.** Only the first typing indicator, before the reply, and that none follows the reply, are asserted.
- **Day-long limits** (contacts over a day old, the 24-hour history cap, the dedupe window ending) need the clock moved; they stay unit tests.
- **Log wording.** Scenarios check only lines with no approved change: the stranger's id with "not in SIGNAL_DM_USER_IDS", "OpenRouter error", "at capacity", "truncated", the cost, "tools are off", "images are off", "signal-cli read failed", signal-cli's error text, and each line's priority. "mention from Alice(id)" is not pinned, as it becomes "received group chat".
- **At `e40dc5f` with `LOG_LEVEL=debug`,** an unaddressed group message that mentions someone whose name the bot doesn't know logs "unresolved mention of <id>" at debug. The spec says an unaddressed message is never logged, so TestScenarioUnaddressedMessagesAreNeverLogged uses a known name. The new core, which always writes debug, must not log it.
