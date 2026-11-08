//go:build nosignal

package main

// A build needs a chat network, and Signal is the only one, so -tags
// nosignal stops the build here, rather than making a bot that starts and
// answers no one. When there is another network, this file's tag becomes
// nosignal && no<that network>.
var _ = this_build_has_no_chat_network_drop_the_nosignal_tag
