# iowncloud

Back up your iPhone to your own NAS instead of iCloud.

iowncloud is a single Docker container with a phone-friendly web app. It makes the same full,
encrypted backups as Finder or iTunes, over Wi-Fi, and keeps previous versions on your disk.
When a backup is due it sends you a reminder (ntfy, Telegram, or any of ~30 services) with a
**Back up now** button. Tap it, type your iPhone passcode, done.

> **Why a button and not a fully automatic nightly backup?** Since recent iOS versions, every
> encrypted backup to a computer must be confirmed with the passcode on the phone. Apple doesn't
> let anyone skip that (iCloud excepted). iowncloud makes that one tap as painless as possible.

## Features

- Full encrypted backups over Wi-Fi in Finder/iTunes format, restorable with Apple's tools
- Previous versions via copy-on-write snapshots (Btrfs/XFS), so a failed backup never eats the last good one
- Reminders when a backup is due and the phone is home, with a one-tap start button:
  - **ntfy**: native action button, nothing opens
  - **Telegram**: inline button; the message updates with live progress
  - **anything else shoutrrr supports**: a one-time link
- Pair once over USB (plugged into the NAS) or by uploading the pairing file from a Mac/PC
- Live progress in the web app; installable on the home screen
- English and Polish UI; adding a language is one TOML file
- No cloud, no account, no telemetry

## Quick start

```bash
docker compose up -d
```

with this `docker-compose.yml` (adjust the backup path):

```yaml
services:
  iowncloud:
    image: ghcr.io/plumthedev/iowncloud:latest
    restart: unless-stopped
    network_mode: host          # required: mDNS discovery
    privileged: true            # optional: USB pairing
    volumes:
      - /dev/bus/usb:/dev/bus/usb
      - ./data:/data
      - /volume1/iphone-backups:/backups
    environment:
      TZ: Europe/Warsaw
```

Open `http://<nas-ip>:8080`, choose a password and add your iPhone.

### Requirements

- **Host networking.** iPhones announce themselves via mDNS, which doesn't pass Docker's bridge.
- **Same network.** The iPhone and the NAS must be on the same LAN (or the phone on VPN for the button).
- **Btrfs or XFS** for the backup folder if you want previous versions. On ext4 you get one copy per device.
- amd64 or arm64.

### Synology

1. In DSM, create a shared folder (e.g. `iphone-backups`) on a Btrfs volume.
2. In Container Manager, create a project from the compose file above.
3. For USB pairing, plug the iPhone into a USB port on the NAS.

## Adding an iPhone

The iPhone must trust the server once:

- **USB:** plug it into the NAS, open *Add iPhone*, tap *Pair*, then *Trust* on the phone.
- **Pairing file:** on a Mac, tick *Show this iPhone when on Wi-Fi* in Finder, then upload
  `/var/db/lockdown/<UDID>.plist`. On Windows the files are in `C:\ProgramData\Apple\Lockdown\`.

Then turn on **backup encryption** on the device page. You pick the password; iowncloud
never stores it. Without encryption, Health data, saved passwords and Wi-Fi settings are skipped.

## Notifications

Add a channel under *Alerts*. Forms are generated for every
[shoutrrr](https://github.com/nicholas-fedor/shoutrrr) service, or paste a shoutrrr URL.

- **ntfy** (recommended for iPhone): self-host ntfy or use ntfy.sh. Self-hosted servers need
  `upstream-base-url: https://ntfy.sh` for instant iOS delivery (only a message ID leaves your network).
- **Telegram**: create a bot with @BotFather and use it **only for iowncloud**. iowncloud
  long-polls the bot for button taps, so the NAS needs no public address.
- **Home Assistant**: send iowncloud's link to the Companion app as an actionable
  notification, or `POST` to the link from a `rest_command`.

Buttons call the *server address* set in *Settings*, so the phone must reach it.

## Restoring

Backups are plain Finder/iTunes backup folders. Copy one from the NAS into
`~/Library/Application Support/MobileSync/Backup/` (Mac) or
`%APPDATA%\Apple Computer\MobileSync\Backup\` (Windows), connect the iPhone and choose
*Restore Backup*. The app's *How to restore* page has the details.

## Configuration

| Variable | Default | |
|---|---|---|
| `IOWNCLOUD_LISTEN` | `:8080` | HTTP address |
| `IOWNCLOUD_DATA` | `/data` | Database and pairing records (**sensitive**: a pairing record grants full access to the phone) |
| `IOWNCLOUD_BACKUPS` | `/backups` | Backup folder |
| `IOWNCLOUD_PASSWORD` | | Sets the admin password on first start |
| `IOWNCLOUD_AUTH` | `on` | `off` disables the login, only behind an authenticating proxy |
| `IOWNCLOUD_MUXER` | `on` | `off` doesn't start netmuxd (development) |
| `TZ` | UTC | Time zone for reminder windows |

## How it works

A Go daemon supervises [netmuxd](https://github.com/jkcoxson/netmuxd), which finds iPhones over
USB and mDNS, and runs [libimobiledevice](https://libimobiledevice.org)'s `idevicebackup2`.
Before each backup the previous one is snapshotted with `cp --reflink`. State lives in SQLite.
The UI is server-rendered Go templates with htmx and Pico CSS, with no Node build step.

## Development

```bash
go test ./...
IOWNCLOUD_MUXER=off IOWNCLOUD_DATA=./tmp/data IOWNCLOUD_BACKUPS=./tmp/backups go run .
```

Requires Go 1.27. Device features need the libimobiledevice tools on `PATH`; the Docker image has them.

## License

AGPL-3.0. libimobiledevice (LGPL-2.1) and netmuxd (LGPL-2.1) are run as separate programs;
their sources are linked from the [Dockerfile](Dockerfile) at the exact versions used.
