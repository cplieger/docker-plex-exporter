// Package wsclient implements the websocket listener that
// connects to Plex's notification channel and dispatches
// playing / transcode-update events into the session
// tracker. Reconnect, backoff, ping/pong, and read-limit
// policy live here.
package wsclient
