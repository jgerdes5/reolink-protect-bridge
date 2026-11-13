# Reolink Protect Bridge

Use a Reolink video doorbell in UniFi Protect as if it were a UniFi doorbell.

The bridge sits between the doorbell and your UniFi console. Protect sees a
regular G4 Doorbell: you get live view, recording, ring notifications with
the call screen, talkback and person/vehicle/animal detections, all in the
Protect app. No ONVIF, no third-party camera entry.

> **Status:** early. Tested with one Reolink Video Doorbell WiFi (D340W) and
> UniFi Protect 7.3.74. Expect rough edges and please open an issue if
> something doesn't work for you.

## What works

- The doorbell shows up in Protect as a native G4 Doorbell and adopts itself
- Live view and 24/7 recording in high and low quality, with audio
- Snapshots and event thumbnails
- Ring button: push notification, call screen and ring in the timeline
- Two-way audio (talkback) from the Protect app and web UI
- Motion events
- Smart detections for person, vehicle and animal, coming from the doorbell's
  own AI, with real bounding boxes that follow the object in live view
- Smart detection zones, line crossing and loitering zones
- These Protect settings are applied to the doorbell:
  - frame rate and bitrate, limited to the values the doorbell offers
  - brightness, contrast, saturation, hue, sharpness, noise reduction
  - mirror and flip, anti-flicker (50/60 Hz)
  - night vision (auto, on, off, custom threshold) and HDR
  - status light (the doorbell's button light)
  - speaker volume and ring volume (0 mutes the doorbell's own chime)
  - privacy masks (up to 4 rectangles); a mask over the whole picture works
    as a privacy mode (see [Privacy mode](#privacy-mode) below)
  - name, date and logo overlay
  - motion detection on/off, sensitivity and zones
  - smart detection types, sensitivity and zones
- H.264 and H.265. With the doorbell on H.265, turn on enhanced encoding
  (H.265) for the camera in Protect; the bridge converts the doorbell's
  low-quality stream, which is always H.264, with ffmpeg
- Quick start of live view and quality switching: when Protect asks for a
  keyframe, the bridge asks the doorbell for one
- Web page for setup, with automatic discovery of the doorbell and the console
- The bridge always sets the doorbell's keyframe interval to 1 second (or as
  close as the doorbell allows) when it starts, so live view starts quickly.
  This also happens with "Apply camera settings from Protect" turned off, and
  the setting stays on the doorbell
- Runs as a Home Assistant add-on, in Docker or as a plain Linux service
- Works with the UniFi Protect integration in Home Assistant, which sees it as
  a normal Protect doorbell

## What doesn't work

- More than one doorbell per bridge, and more than one bridge per IP address
  (Protect always sends talkback to UDP port 7004)
- Custom ringtones from Protect: the doorbell keeps playing its own chime
- Face recognition and licence plate detection: the doorbell's AI doesn't
  provide them
- Package detection and audio detections (smoke alarm, glass break, ...)
- These Protect settings have no counterpart on the doorbell and are ignored:
  - IR LED intensity
  - status sounds
  - microphone volume
  - lens distortion correction
  - date format of the overlay
- Privacy masks are rectangles only; free-form shapes are turned into their
  bounding box
- Displays and LCD messages: the Reolink doorbell has no display
- HomeKit talkback through Protect
- Protect updates can change the camera protocol at any time

## How it works

```
Reolink doorbell  ── Baichuan (TCP 9000) ──▶  Reolink Protect Bridge  ── UniFi camera protocol ──▶  UniFi Protect
                  ◀── settings, talkback ──                           ◀── settings, talkback ──
```

The bridge talks to the doorbell over Reolink's own protocol (Baichuan) for
video, events and settings. Towards Protect it behaves like a UniFi camera: it
connects to the console, gets adopted, and streams the doorbell's video in the
format Protect expects. Video and the doorbell's AAC audio are passed through,
not re-encoded, so it runs fine on small hardware. Two things go through
ffmpeg:

- Opus audio. UniFi cameras send Opus next to AAC because Protect's live view
  plays Opus; the doorbell can only do AAC, so ffmpeg converts it (a percent or
  two of one CPU core). Without it Protect likely has to convert the audio
  itself whenever live view opens, which would delay the picture.
- When Protect wants H.265 (enhanced encoding), the low-quality stream, which
  the doorbell only sends as H.264, is converted to H.265. That costs some CPU.

The bridge opens the connections to Protect itself. Snapshot requests and
settings come back over that connection, so the only thing Protect sends to
the bridge on its own is talkback audio (UDP 7004).

## Requirements

- A Reolink video doorbell (H.264 or H.265)
- UniFi Protect 7.x on a UniFi console (Dream Machine, Cloud Key, UNVR, ...)
- A host on the same network that runs 24/7 and has a **fixed IP address**.
  Protect remembers the camera by that address and sends talkback audio to it.
- One of these, so the bridge can adopt the camera:
  - a local UniFi OS user without SSO or 2FA, with full Protect access (the
    bridge then fetches adoption tokens itself), or
  - an adoption token from Protect. Without the user, the ring method
    "Protect API" is not available.

## Installation

### Home Assistant add-on

1. In Home Assistant go to **Settings → Add-ons → Add-on Store**, open the
   menu (⋮) → **Repositories** and add
   `https://github.com/jgerdes5/reolink-protect-bridge`.
2. Install **Reolink Protect Bridge** and start it.
3. Open **Reolink Protect** in the sidebar.

The camera uses the Home Assistant host's IP in Protect. Bonus: the UniFi
Protect integration in Home Assistant picks the doorbell up like any other
Protect doorbell, so you can use its ring and detection events in automations.

### Docker

```sh
git clone https://github.com/jgerdes5/reolink-protect-bridge
cd reolink-protect-bridge/deploy
mkdir data && sudo chown 10001:10001 data
docker compose up -d
```

The data folder has to be next to `docker-compose.yml` (in `deploy/`), because
the compose file mounts `./data` relative to its own location. The container
runs as user 10001, hence the `chown`.

The container needs host networking (talkback arrives on UDP 7004 and
discovery uses broadcasts). The web page is on port 8080; you can change the
port on the web page under Options, then restart the container.

### Linux / Proxmox LXC

On Debian or Ubuntu:

```sh
curl -fsSLO https://raw.githubusercontent.com/jgerdes5/reolink-protect-bridge/main/deploy/install.sh
sudo sh install.sh
```

This installs the binary, ffmpeg and a systemd service. For a new Proxmox
container there's a step-by-step guide in [deploy/proxmox.md](deploy/proxmox.md).
Prebuilt binaries are also attached to each
[release](https://github.com/jgerdes5/reolink-protect-bridge/releases).

## Setup

1. Open the web page at `http://<host>:8080` and set an admin password.
   `<host>` is the IP address of the machine the bridge runs on, for example
   `192.168.1.50`. The install script prints the full address at the end; on
   the machine itself `hostname -I` shows it (first entry). Use the IP address
   (or a local name like `bridge.local`) rather than a public domain name, the
   page refuses to create the admin password otherwise. In Home Assistant
   just open it from the sidebar, no extra password needed.
2. Under **Connections**, enter the doorbell (or use *Find doorbells*) and your
   UniFi console with the local user or an adoption token.
3. Save. After a few seconds the doorbell shows up in Protect and adopts itself.
4. In Protect, turn on motion detection and set up smart detections and zones
   as you like.

If the doorbell is already in Protect via ONVIF, remove that entry first,
otherwise you'll have it twice.

The **Options** page has:

- the camera name in Protect and the ring method ("Camera" sends the ring over
  the camera connection, "Protect API" uses the Protect user)
- switches for events, talkback and applying Protect's camera settings to the
  doorbell
- the admin password (not shown in Home Assistant, which handles the login)
- under Advanced, the web page address (takes effect after a restart of the
  app) and the path to ffmpeg (the file must be called `ffmpeg`)
- the camera identity: a new one makes Protect see a brand-new camera that has
  to be adopted again

If you change the doorbell or console address, enter its password (and
adoption token, if you use one) again. Stored passwords are only sent to the
address they were entered for.

The console's certificate is trusted on the first login (most consoles use a
self-signed one) and its key is shown on the Connections page. After that the
Protect password is only sent to a console with the same key; a certificate
that is valid for the console's name under the usual public authorities (e.g.
Let's Encrypt) is always accepted. If you replace the console's self-signed
certificate, tick "Trust the certificate the console shows next" and save.

## Privacy mode

A privacy mask over the whole picture in Protect works as a privacy mode: the
doorbell blacks out the video. It only hides the picture. The doorbell's
microphone keeps running and its audio still reaches Protect (live view and
recordings). Whether the ring button and detections behave normally while the
picture is masked hasn't been tested yet.

## Ports

- Bridge → doorbell: TCP 9000
- Bridge → UniFi console: TCP 443, 7442, 7444 and 7550
- UniFi console → bridge: UDP 7004 (talkback)
- Browser → bridge: TCP 8080 by default (web page, changeable under Options;
  not used with the Home Assistant add-on)

## Troubleshooting

- **Camera doesn't appear in Protect:** check the log on the status page.
  "protect login: HTTP 401/403" means the user is wrong, uses SSO/2FA or lacks
  Protect access. "the console's certificate changed" means the console showed
  a different certificate than on the first login: if you replaced it
  yourself, trust the new one on the Connections page; otherwise something on
  your network may be pretending to be the console.
- **Live view stutters or recordings have gaps:** make sure the host's clock is
  synced (NTP).
- **No talkback:** check that UDP 7004 from the console to the bridge isn't
  blocked. Remove and re-adopt the camera if Protect doesn't offer the speaker.
- **No video after setting the doorbell to H.265:** restart the bridge (it
  reads the codec when it starts), then turn on enhanced encoding (H.265) for
  the camera in Protect. The log says "Protect expects H.264 but the
  doorbell's main stream is H.265" until both match.
- **Ring arrives in the log but not in Protect:** switch the ring method to
  "Protect API" under Options.

## Development

```sh
go test -race ./...
go build ./cmd/reolink-protect-bridge
```

The video and audio handling uses [go2rtc](https://github.com/AlexxIT/go2rtc)
as a regular Go dependency, so it can be updated on its own. Most of the
interesting code is in `internal/bridge`, `pkg/unifi` and `pkg/baichuan`.

## Credits

This project wouldn't exist without:

- [go2rtc](https://github.com/AlexxIT/go2rtc) by Alexey Khit, used for the media handling
- [fzurita/go2rtc](https://github.com/fzurita/go2rtc) by Francisco Zurita, whose
  Baichuan and Reolink code is the base of `pkg/baichuan` and `pkg/reolink`
- [unifi-cam-proxy](https://github.com/keshavdv/unifi-cam-proxy) by Keshav Varma
  and the Reolink fork
  [reolink-unifi-protect](https://github.com/woutersteegers/reolink-unifi-protect),
  which figured out much of how Protect talks to cameras
- [reolink_aio](https://github.com/starkillerOG/reolink_aio), a great reference
  for the Baichuan protocol
- [pyunifiwire](https://github.com/rjmotion/pyunifiwire) and
  [unifi-guides](https://github.com/rjmotion/unifi-guides), for their
  measurements of the UniFi protocol

Licence details for the included code are in
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

## Disclaimer

This project is not affiliated with or endorsed by Ubiquiti or Reolink. All
product names are trademarks of their respective owners. Use it at your own
risk.

Most of the code and documentation in this repository was written with the
help of AI (Claude by Anthropic).

## License

[MIT](LICENSE)
