# Spike: does the stack work on Synology?

Goal: prove the four assumptions before writing any daemon code.

1. USB pairing works inside a container on DSM.
2. Wi-Fi backup works via netmuxd with that pair record.
3. Reflink snapshots work on the bind-mounted backup folder.
4. netmuxd handles USB and network at once (no separate usbmuxd).

The image contains libimobiledevice tools, `netmuxd` and `idevicewifi`
(our helper that toggles "Show this iPhone when on Wi-Fi").

## 0. Prepare

On the NAS (SSH, admin user):

```sh
uname -m          # x86_64 -> use amd64 tar, aarch64 -> arm64 tar
```

In DSM create a shared folder `iphone-backups` on a **Btrfs** volume.

Copy the matching tar from `dist/` to the NAS, then:

```sh
sudo docker load -i iowncloud-spike-amd64.tar
sudo mkdir -p /volume1/docker/iowncloud/data
sudo docker run -d --name iowncloud-spike \
  --network host --privileged \
  -v /dev/bus/usb:/dev/bus/usb \
  -v /volume1/docker/iowncloud/data:/data \
  -v /volume1/iphone-backups:/backups \
  iowncloud:spike
sudo docker logs iowncloud-spike
```

Shortcut used below: `x() { sudo docker exec -it iowncloud-spike "$@"; }`

## 1. USB pairing

Plug the iPhone into a NAS USB port, unlock it.

```sh
x idevice_id -l                # expect: <UDID> (USB)
x idevicepair pair             # tap "Trust" + passcode on the phone, run again until SUCCESS
x idevicewifi on
x idevicewifi status           # expect: on
x ideviceinfo -k DeviceName
ls -l /volume1/docker/iowncloud/data/lockdown/   # expect a <UDID>.plist
```

If `idevice_id -l` prints nothing: `sudo docker logs iowncloud-spike` and `lsusb` on the host.

## 2. Wi-Fi backup

Unplug the cable. Keep the phone unlocked on the same Wi-Fi, ideally on a charger.

```sh
x idevice_id -n                # expect: <UDID> (Network), may take ~30 s
U=<UDID>
x idevicebackup2 -n -u $U info
x idevicebackup2 -n -u $U encryption on 'YOUR-TEST-PASSWORD'   # save it in your password manager
```

First full backup takes long; run it detached so an SSH drop doesn't kill it:

```sh
sudo docker exec -d iowncloud-spike sh -c \
  "idevicebackup2 -n -u $U backup /backups > /backups/spike.log 2>&1; echo exit=\$? >> /backups/spike.log"
tail -f /volume1/iphone-backups/spike.log
```

When done, run the same command again: the second (incremental) run should be much faster. Note both durations.

## 3. Reflink snapshot

```sh
x sh -c "time cp -a --reflink=always /backups/$U /backups/$U.snap && du -sh /backups/$U"
df -h /volume1                 # used space should NOT grow by the backup size
x rm -rf /backups/$U.snap
```

## 4. USB + network together

Plug the phone back in while Wi-Fi is on:

```sh
x idevice_id                   # expect the UDID listed (USB and/or Network), no errors in docker logs
```

## Report back

Paste: `uname -m`, DSM version, iOS version, output of each step (or the first error),
both backup durations and the backup size, result of step 3 (time + whether `df` grew).

Cleanup: `sudo docker rm -f iowncloud-spike`

## Results (2026-10-26, DS224+ x86_64, DSM 7.4.1, kernel 4.4.302+, iPhone14,4 iOS 27.0.1)

| Check | Result |
|---|---|
| USB in container | OK. netmuxd sees the phone via nusb with `--privileged -v /dev/bus/usb`; no usbmuxd needed. |
| Pairing | OK. `idevicepair pair` SUCCESS, record in `/data/lockdown`. `idevicewifi on` works. |
| Wi-Fi discovery | OK. `idevice_id -n` lists the phone a few seconds after unplugging. |
| Full backup over Wi-Fi | OK. 9.2 GB, 30,866 files, 829 s. |
| Incremental, phone unlocked | OK. 75 files, 128 s. |
| Incremental, phone locked on charger | **FAIL.** `Waiting for passcode` then `ErrorCode 208: Device locked` after ~70 s. |
| Reflink snapshot | OK. 512 MB in 24 ms, snapshot isolated from later writes. |
| USB + network at once | Partly. Both transports were active in one netmuxd; not explicitly re-tested. |

Findings:

- **Every encrypted backup requires the passcode typed on the phone** (seen on all 3 incremental attempts). Unattended nightly encrypted backups are impossible on iOS 27; the schedule model must change.
- Pair record lands as `rwxrwxrwx` because Synology share ACLs override the mode; the daemon must not rely on `chmod 0600` on a share.
- Docker socket on DSM is `root:root 660`; ssh user needs `chgrp administrators /var/run/docker.sock` (resets on restart).
- netmuxd logs `No pairing record found for "<UDID without dash>"` once but still serves the device; cosmetic.
- `idevicebackup2` progress output buffers when redirected; the runner must read it via a pty or tolerate bursts.
