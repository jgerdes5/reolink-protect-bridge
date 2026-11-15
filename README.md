# Reolink Protect Bridge

Use a Reolink video doorbell in UniFi Protect as a native G4 Doorbell.

The bridge talks to the doorbell over Reolink's own protocol and to your UniFi
console as a UniFi camera. Protect adopts it like any other G4 Doorbell, so
live view, recording, ring notifications, talkback and smart detections all
work in the Protect app, without ONVIF or a third-party camera entry.

> [!NOTE]
> This is an early release, only tested with the Reolink Video Doorbell WiFi
> and UniFi Protect 7.3. Reports from other models and Protect versions are
> welcome.

## Why

UniFi Protect can add a Reolink doorbell as a third-party camera over ONVIF,
but it never feels like part of the system. Live view and recordings stutter,
rings and detections don't show up as events, and the whole experience falls
well short of Ubiquiti's own cameras. I wanted the doorbell to behave like
every other camera in Protect, so this bridge makes it look like a real G4
Doorbell instead.

## Supported doorbells

Only Reolink doorbells are supported for now. Other Reolink cameras might be
supported in the future.

| Doorbell | Status |
|----------|--------|
| Reolink Video Doorbell WiFi | Tested |
| Reolink Video Doorbell PoE | Should work, untested. Same camera and features as the WiFi model, only the network connection differs |
| Reolink Video Doorbell (Battery), 2nd Gen, in Wired Power Mode | Might work, untested. In this mode it runs like a plugged-in camera with continuous recording |
| Battery doorbells running on battery | Untested, may not work |

Protect expects a camera to stream around the clock, while a battery doorbell
sleeps between events to save power. Even if it works, keeping it streaming
would likely drain the battery within days.

## Features

- Adopts itself in Protect as a G4 Doorbell
- Live view and continuous recording in high and low quality, with audio
- Ring button with push notification and call screen
- Two-way audio from the Protect app, the web interface and HomeKit
- Motion events and smart detections (person, vehicle, animal) from the
  doorbell's own AI, with bounding boxes in live view. Package detection is
  passed on too, on doorbells that have it
- Detection zones, exclusion zones, line crossing and loitering
- Privacy masks
- Protect's camera settings are applied to the doorbell: image, night vision,
  HDR, status light, volume, overlay, frame rate and bitrate
- Web interface with automatic discovery of the doorbell and the console
- Runs as a Home Assistant add-on, in Docker or as a systemd service
- Shows up in Home Assistant's UniFi Protect integration like any other
  Protect doorbell, ready for automations

## Limitations

- One doorbell per bridge, and one bridge per IP address
- Custom ringtones from Protect aren't supported; the doorbell plays its own
  chime
- No face recognition, licence plate or audio detections
- Privacy masks are limited to 4 rectangles
- A few Protect settings have no equivalent on the doorbell and are ignored,
  such as IR LED brightness, microphone volume and status sounds
- Protect updates can change the camera protocol at any time

## How it works

```
Reolink doorbell ◀── Baichuan ──▶ Reolink Protect Bridge ◀── UniFi camera protocol ──▶ UniFi Protect
```

Video and the doorbell's AAC audio are passed through without re-encoding, so
the bridge runs fine on small hardware. ffmpeg handles the rest: an Opus copy
of the audio, which lets Protect's web player use WebRTC like it does for
UniFi cameras, decoding talkback audio (AAC, or Opus from HomeKit) for the
doorbell, and snapshots.

The bridge opens all connections to the console itself. The only traffic
Protect starts on its own is talkback audio.

## Requirements

- A Reolink video doorbell with continuous power (see
  [Supported doorbells](#supported-doorbells))
- UniFi Protect on a UniFi console (Dream Machine, Cloud Key, UNVR, ...),
  tested with version 7.3
- An always-on host on the same network with a **fixed IP address**. Protect
  remembers the camera by this address.
- A local UniFi OS user without SSO or 2FA and with full Protect access, or an
  adoption token from Protect

## Installation

### Home Assistant add-on

Requires Home Assistant OS, where add-ons are called apps since version 2026.2.

1. Go to **Settings → Apps → Install app**, open the menu (⋮) →
   **Repositories** and add `https://github.com/jgerdes5/reolink-protect-bridge`.
   On older versions this is under **Settings → Add-ons → Add-on Store**.
2. Install **Reolink Protect Bridge** and start it.
3. Open **Reolink Protect** in the sidebar.

The camera uses the Home Assistant host's IP address in Protect.

### Docker

```sh
git clone https://github.com/jgerdes5/reolink-protect-bridge
cd reolink-protect-bridge/deploy
mkdir data && sudo chown 10001:10001 data
docker compose up -d
```

The container needs host networking, because talkback arrives on UDP 7004 and
finding the console uses a broadcast. The web interface listens on port 8080.

### Linux

The bridge is a single binary and runs on any Linux with ffmpeg. The install
script sets it up as a systemd service:

```sh
curl -fsSLO https://raw.githubusercontent.com/jgerdes5/reolink-protect-bridge/main/deploy/install.sh
sudo sh install.sh
```

On Debian and Ubuntu the script installs ffmpeg itself; on other distributions,
install ffmpeg (with Opus support, e.g. libopus) and curl with your package
manager first. Running the script
again updates to the latest release. For Proxmox there's a step-by-step guide in
[deploy/proxmox.md](deploy/proxmox.md). Prebuilt binaries are attached to each
[release](https://github.com/jgerdes5/reolink-protect-bridge/releases).

## Setup

1. Open `http://<host>:8080`, where `<host>` is the IP address of the machine
   running the bridge (for example `192.168.1.50`), and create an admin
   password. In Home Assistant, open it from the sidebar instead.
2. Under **Connections**, enter the doorbell (or let the bridge find it) and
   your UniFi console with the local user or an adoption token.
3. Save. The doorbell appears in Protect and adopts itself after a few seconds.
4. Set up motion detection, smart detections and zones in Protect.

If the doorbell is already in Protect through ONVIF, remove that entry first.

The bridge sets the doorbell's keyframe interval to one second, which Protect
needs for quick live view. Everything else on the doorbell is only changed when
"Apply camera settings" is on.

### Console certificate

The console's certificates are trusted on first use: the Protect password and
the camera connection then only go to a console with the same keys. If you
replace the console's certificate or reset the console, trust the new one on
the **Connections** page.

### Privacy mode

Draw an exclusion zone over the whole picture in Protect and tick **Privacy
mask**. The doorbell blacks out the video; its microphone keeps recording.

## Ports

| Direction | Port |
|-----------|------|
| Bridge → doorbell | TCP 9000 |
| Bridge → console | TCP 443, 7442, 7444, 7550 |
| Console → bridge | UDP 7004 (talkback) |
| Browser → bridge | TCP 8080 (web interface, configurable) |

To find devices, the bridge scans the local network for doorbells on TCP 9000
and looks for the console with a UDP broadcast on port 10001.

## Troubleshooting

- **The camera doesn't show up in Protect.** Check the log on the
  **Status** page. `HTTP 401/403` on login means the user is wrong, uses SSO
  or 2FA, or lacks Protect access.
- **"The console's certificate changed"** (or the certificate of Protect's
  camera connection). If you replaced the certificate or reset the console,
  trust the new one on the **Connections** page. Otherwise something on your
  network may be impersonating the console.
- **Recordings have gaps.** Make sure the host's clock is synced over NTP.
- **No talkback.** Check that UDP 7004 from the console to the bridge isn't
  blocked.
- **The Status page shows "Audio: AAC only".** The ffmpeg in use has no Opus
  encoder. Install an ffmpeg with libopus or point the bridge to one under
  **Options → Advanced**. Without Opus, live view goes black briefly when you
  switch quality and HomeKit talkback doesn't work.
- **Rings show up in the log but not in Protect.** Set the ring method to
  "Protect API" under **Options**.

When reporting a problem, turn on **Debug log** under **Options → Advanced**
and include the log from the **Status** page.

## Development

```sh
go test -race ./...
go build ./cmd/reolink-protect-bridge
```

Media handling uses [go2rtc](https://github.com/AlexxIT/go2rtc) as a library.
Most of the code lives in `internal/bridge`, `pkg/unifi` and `pkg/baichuan`.

## Credits

Built on the work of:

- [go2rtc](https://github.com/AlexxIT/go2rtc) by Alexey Khit, for media handling
- [fzurita/go2rtc](https://github.com/fzurita/go2rtc) by Francisco Zurita, the
  base of `pkg/baichuan` and `pkg/reolink`
- [unifi-cam-proxy](https://github.com/keshavdv/unifi-cam-proxy) by Keshav Varma
  and its Reolink fork
  [reolink-unifi-protect](https://github.com/woutersteegers/reolink-unifi-protect),
  which worked out much of how Protect talks to cameras
- [reolink_aio](https://github.com/starkillerOG/reolink_aio), a reference for
  the Baichuan protocol
- [pyunifiwire](https://github.com/rjmotion/pyunifiwire) and
  [unifi-guides](https://github.com/rjmotion/unifi-guides), for their notes on
  the UniFi protocol

Licences of included code are listed in
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

## Disclaimer

This project is not affiliated with or endorsed by Ubiquiti or Reolink. All
product names are trademarks of their respective owners. Use at your own risk.

Most of the code and documentation was written with the help of AI (Claude by
Anthropic).

## License

[MIT](LICENSE)
