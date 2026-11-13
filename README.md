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
    as a privacy mode, while the ring button and events keep working
  - name, date and logo overlay
  - motion detection on/off, sensitivity and zones
  - smart detection types, sensitivity and zones
- H.264 and H.265. With the doorbell on H.265, turn on enhanced encoding
  (H.265) for the camera in Protect; the bridge converts the doorbell's
  low-quality stream, which is always H.264, with ffmpeg
- Quick start of live view and quality switching: when Protect asks for a
  keyframe, the bridge asks the doorbell for one
- Web page for setup, with automatic discovery of the doorbell and the console
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
format Protect expects. Video and audio are passed through, not re-encoded, so
it runs fine on small hardware.

## Requirements

- A Reolink video doorbell (H.264 or H.265)
- UniFi Protect 7.x on a UniFi console (Dream Machine, Cloud Key, UNVR, ...)
- A host on the same network that runs 24/7 and has a **fixed IP address**.
  Protect sends snapshot requests and talkback audio to that address.
- A local UniFi OS user without SSO or 2FA, with full Protect access (used to
  adopt the camera)

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
cd reolink-protect-bridge
mkdir data && sudo chown 10001 data
docker compose -f deploy/docker-compose.yml up -d
```

The container needs host networking (talkback arrives on UDP 7004 and
discovery uses broadcasts). The web page is on port 8080.

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

1. Open the web page at `http://<host>:8080` and set an admin password. In
   Home Assistant just open it from the sidebar, no extra password needed.
2. Under **Connections**, enter the doorbell (or use *Find doorbells*) and your
   UniFi console with the local user.
3. Save. After a few seconds the doorbell shows up in Protect and adopts itself.
4. In Protect, turn on motion detection and set up smart detections and zones
   as you like.

If the doorbell is already in Protect via ONVIF, remove that entry first,
otherwise you'll have it twice.

The **Options** page lets you change the camera name and turn talkback, events and settings sync on or off.

## Ports

- Bridge → doorbell: TCP 9000
- Bridge → UniFi console: TCP 443, 7442, 7444 and 7550
- UniFi console → bridge: UDP 7004 (talkback)
- Browser → bridge: TCP 8080 (web page, not used with the Home Assistant add-on)

## Troubleshooting

- **Camera doesn't appear in Protect:** check the log on the status page.
  "protect login: HTTP 401/403" means the user is wrong, uses SSO/2FA or lacks
  Protect access.
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
