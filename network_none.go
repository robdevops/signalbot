//go:build nosignal && notelegram

package main

// A build needs a chat network, so -tags nosignal,notelegram stops the
// build here, rather than making a bot that starts and answers no one.
// Each new network adds its tag to this file's.
var _ = this_build_has_no_chat_network_drop_nosignal_or_notelegram
