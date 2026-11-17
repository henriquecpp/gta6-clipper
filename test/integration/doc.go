// Package integration holds the end-to-end pipeline test (build tag
// `integration`): real capture, SQLite and ffmpeg render, with fakes for
// Twitch, Telegram, Claude and whisper. It runs inside the app test container
// (`task test:integration`) because it needs ffmpeg.
package integration
