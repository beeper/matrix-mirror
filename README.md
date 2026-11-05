# matrix-mirror

Keeps a local, searchable copy of a Matrix account's chats. It logs in as its own verified device, syncs and decrypts, and writes chats and messages to SQLite with full-text search. A read-only CLI with stable JSON output sits on top.

It works with any homeserver. On Beeper, chats bridged through Beeper Cloud are Matrix rooms too, so one mirror covers them. Chats connected on-device never reach Beeper's servers, so they aren't mirrored.

It runs on macOS and Linux.

## Install

```sh
go install -tags goolm github.com/beeper/matrix-mirror/cmd/matrix-mirror@latest
```

`-tags goolm` is required: it selects mautrix's pure-Go Olm. Without it the build needs libolm and cgo.

To upgrade, run the same command, then `matrix-mirror install` to restart the background service onto the new binary.

## Set up

```sh
matrix-mirror login --beeper                      # Beeper: email + code
matrix-mirror login --homeserver matrix.org       # any homeserver: password, SSO or --token
matrix-mirror verify                              # recovery key -> cross-signed device + key backup
matrix-mirror install                             # run `sync` in the background (or `matrix-mirror sync` in a terminal)
```

`login` always creates a new device, named "matrix-mirror on \<this machine\>", so you can spot it and revoke it from any client. `verify` asks for your recovery key and uses it once; the key is never written to disk.

`install` points the service at the binary you run it from:

- **macOS:** a LaunchAgent that starts at login.
- **Linux:** a systemd user service. It starts at login, or at boot once you've run `loginctl enable-linger`.

`uninstall` stops and removes the service.

## Read

```sh
matrix-mirror status
matrix-mirror chats [--unread] [--network whatsapp] [--limit 50]
matrix-mirror read "Alice" [--since 7d] [--before 2026-11-01] [--limit 50]
matrix-mirror search "dinner friday" [--chat Alice] [--since 2026-10-01] [--limit 20]
matrix-mirror unread
```

Add `--json` to any of these for machine-readable output. Every JSON object carries a top-level `"v": 1`, and within a version the shapes only change additively. On failure, `--json` prints `{"v": 1, "error": {"code": ..., "message": ...}}` and exits 1. `<chat>` is a room id or a unique match on the chat name. The read commands open the database read-only, so they work while sync is running.

## Data

All data lives in `~/.matrix-mirror` (override with `--home` or `MATRIX_MIRROR_HOME`). Directories are 0700 and files 0600.

| Path | Contents |
|---|---|
| `mirror.db` | Chats, members, messages and the FTS index. It holds decrypted copies of your messages. |
| `crypto.db` | mautrix's crypto store: this device's keys and room keys. |
| `secrets/session.json` | Access token, device id and the crypto store's pickle key. |
| `logs/` | `sync.log` (rotated). On macOS, launchd's output goes to `launchd.log`; on Linux it goes to the journal (`journalctl --user -u matrix-mirror`). |

`logout` deletes the device and its keys but keeps `mirror.db`.

## Develop

```sh
go test -tags goolm ./...     # unit tests
scripts/e2e.sh                # end-to-end test against a throwaway Synapse in Docker
```
