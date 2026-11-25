//go:build nosignal && notelegram && nodiscord && noslack

package main

// A build needs a chat network, so leaving out every one (-tags
// nosignal,notelegram,nodiscord,noslack) stops the build here, rather than
// making a bot that starts and answers no one. Each new network adds its
// tag to this file's.
var _ = this_build_has_no_chat_network_keep_one_of_signal_telegram_discord_or_slack
