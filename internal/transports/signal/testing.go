package signal

// SetSelf sets the bot's account id without asking signal-cli: for tests
// and fakes.
func (s *Client) SetSelf(id string) { s.selfID.Store(id) }

// SetContacts replaces the known contacts as listContacts would: for tests.
func (a *Adapter) SetContacts(cs []Contact) { a.names.replaceContacts(cs) }
