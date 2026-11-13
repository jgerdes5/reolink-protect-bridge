# Reolink Protect Bridge

Use a Reolink video doorbell in UniFi Protect as if it were a UniFi doorbell.

The bridge sits between the doorbell and your UniFi console. Protect sees a
regular G4 Doorbell: you get live view, recording, ring notifications with
the call screen, talkback and person/vehicle/animal detections, all in the
Protect app. No ONVIF, no third-party camera entry.

> **Status:** early. Tested with one Reolink Video Doorbell WiFi (D340W) and
> UniFi Protect 7.3.74. Expect rough edges and please open an issue if
> something doesn't work for you.

## Features

- Shows up in Protect as a native doorbell (G4 Doorbell, or G5 Pro if you prefer)
- Live view and 24/7 recording in high and low quality, with audio
- Ring button: notification, call screen and ring in the timeline
- Two-way audio from the Protect app
- Motion and smart detections (person, vehicle, animal) from the doorbell's
  own AI, with real bounding boxes, thumbnails, zones, line crossing and
  loitering
- Camera settings in Protect are applied to the doorbell: picture, night
  vision, HDR, mirror/flip, status light, volumes, detection zones and
  sensitivity
- Small web page for setup, with automatic discovery of the doorbell and the
  console
- Runs as a Home Assistant add-on, in Docker or as a plain Linux service

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

- A Reolink doorbell set to **H.264** (H.265 isn't supported yet)
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

The **Options** page lets you change the camera name, the model it shows up as,
and turn talkback, events, settings sync and fast quality switching on or off.

## Ports

| From | To | Port |
|---|---|---|
| Bridge | Doorbell | TCP 9000 |
| Bridge | UniFi console | TCP 443, 7442, 7444, 7550 |
| UniFi console | Bridge | UDP 7004 (talkback) |
| Browser | Bridge | TCP 8080 (web page, not used with the add-on) |

## Limitations

- One doorbell per bridge (and per IP address, because of the fixed talkback
  port)
- H.264 only
- No custom ringtones from Protect; the doorbell keeps its own chime
- Face and licence plate recognition don't work, the doorbell's AI doesn't
  provide them
- Switching between high and low quality in Protect can show a short black
  frame. Real UniFi cameras produce a keyframe on demand, Reolink can't. The
  *fast quality switching* option halves the delay.
- Protect updates can change the protocol at any time

## Troubleshooting

- **Camera doesn't appear in Protect:** check the log on the status page.
  "protect login: HTTP 401/403" means the user is wrong, uses SSO/2FA or lacks
  Protect access.
- **Live view stutters or recordings have gaps:** make sure the host's clock is
  synced (NTP).
- **No talkback:** check that UDP 7004 from the console to the bridge isn't
  blocked. Remove and re-adopt the camera if Protect doesn't offer the speaker.
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
