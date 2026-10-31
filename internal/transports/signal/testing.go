package signal

import ()

// SetSelf sets the bot's account id without asking signal-cli: for tests
// and fakes.
func (s *Client) SetSelf(id string) { s.selfID.Store(id) }

// Network is "tcp" for a host:port address, else "unix".
func (s *Client) Network() string { return s.network() }

// Dispatch handles one frame as if read from the socket: for tests.
func (s *Client) Dispatch(line []byte, handle func(*Envelope)) { s.dispatch(line, handle) }
