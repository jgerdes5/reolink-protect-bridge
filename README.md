# Reolink Protect Bridge

Reolink Protect Bridge makes a **Reolink video doorbell** appear in **UniFi
Protect** as a native UniFi doorbell. Protect adopts it like its own hardware:
live view, 24/7 recording, the timeline, ring notifications with the call
screen, talkback, and person/vehicle/animal detections all work. It does this
without ONVIF or a third-party camera entry.

It is a single program with a small web page for setup. It connects to the
doorbell over Reolink's own protocol (Baichuan, TCP 9000) and to Protect over
UniFi's camera protocol (`ubnt_avclient`). Video and audio are passed through
without transcoding.

Tested with the Reolink Video Doorbell WiFi (D340W, firmware v3.0.0.4662) and
UniFi Protect 7.3.74. Other Reolink doorbells that speak Baichuan are likely to
work.

> Not affiliated with, endorsed by or supported by Ubiquiti or Reolink.

## What works

| In Protect | From the doorbell |
|---|---|
| Camera adopted as **G4 Doorbell** (or G5 Pro), doorbell flag | own UniFi identity: generated MAC and certificate |
| Live view and recording in three qualities | main stream (e.g. 2560×1920, 20 fps) → High; sub stream → Medium and Low |
| Audio | the doorbell's AAC stream |
| **Ring**: notification, call screen, ring in the timeline | button press (Baichuan push event), sent as a real G4 Doorbell sends it |
| Motion events | Reolink motion detection |
| **Smart detections** person / vehicle / animal, with thumbnails and crops | the doorbell's own AI, with its **real detection boxes** |
| Smart-detect zones, line crossing | evaluated by the bridge against the doorbell's boxes |
| **Talkback** from the app and the web UI | Protect's AAC audio is decoded and played on the doorbell's speaker |
| Snapshots | keyframe from the main stream |
| Settings: picture (brightness, contrast, …), mirror/flip, night vision, HDR, status light, speaker and ring volume, smart-detection types, sensitivity, motion and smart zones | written to the doorbell (zones become its detection areas) |
| Fast quality switching | keyframe interval set to 0.5 s |

Not supported: H.265 streams (set the doorbell to H.264), a display (the Reolink
has none), Protect's custom ringtones (the doorbell plays its own tone), face and
licence-plate recognition, HomeKit's Opus talkback.

## Install

You need a Linux host on the same network as the doorbell and the UniFi console,
with a **fixed IP address**. Protect sends snapshot requests and talkback audio
to the camera's address. ffmpeg must be installed (the install methods below
handle it). One instance serves one doorbell.

### Proxmox LXC or any Debian/Ubuntu machine

```sh
curl -fsSLO https://raw.githubusercontent.com/jgerdes5/reolink-protect-bridge/main/deploy/install.sh
sudo sh install.sh
```

While the repository is private, the script needs a GitHub token with read
access (`GITHUB_TOKEN=... sh install.sh`); see the guide below.

The script installs ffmpeg, a service user and a systemd service, and prints the
web page address. A step-by-step guide for a new Proxmox container, with every
value explained, is in [deploy/proxmox.md](deploy/proxmox.md). To build from a
checkout instead: `sudo sh deploy/install.sh --from-source .`.

### Home Assistant add-on

On Home Assistant OS or Supervised: Settings → Add-ons → Add-on Store → ⋮ →
Repositories → add `https://github.com/jgerdes5/reolink-protect-bridge`, then
install **Reolink Protect Bridge** and open it from the sidebar. The camera gets
the Home Assistant host's IP address in Protect (keep it fixed), and the web
page is served only through Home Assistant, which handles the login. The
UniFi Protect integration in Home Assistant then shows the doorbell with its
ring and detection events. Details: [add-on documentation](reolink_protect_bridge/DOCS.md).
(Home Assistant can only add the repository once it is public.)

### Docker

```sh
mkdir data && sudo chown 10001 data
docker compose -f deploy/docker-compose.yml up -d
```

The container needs `network_mode: host`. Protect sends talkback to UDP port
7004 on the camera's address, and discovery uses broadcasts.

### Binary

Release binaries for linux/amd64, arm64 and armv7 are attached to each
[release](https://github.com/jgerdes5/reolink-protect-bridge/releases). Run
`reolink-protect-bridge -data /var/lib/reolink-protect-bridge`.

## Setup

1. Open `http://<host>:8080` and create the admin password.
2. **Connections**:
   - **Doorbell**: its address (or *Find doorbells*), user and password.
   - **Protect**: the console's address (or *Find Protect console*) and a
     **local** UniFi OS user (no SSO, no MFA) with full Protect management,
     used to fetch adoption tokens. Alternatively paste an adoption token from
     `https://<console>/proxy/protect/api/cameras/manage-payload`.
3. Save. The camera appears in Protect and adopts itself.
4. In Protect, open the camera's settings: switch on motion detection and set
   up the smart detections and zones you want. The bridge passes them to the
   doorbell.

If the doorbell is already in Protect as an ONVIF/third-party camera, remove
that entry, otherwise it shows up twice. The **Options** page has the camera
name, the identity (G4 Doorbell or G5 Pro), the ring method, and switches for
events, talkback, applying Protect's settings and fast quality switching.
Changing the identity makes Protect see a new camera.

Network ports:

| From | To | Port |
|---|---|---|
| bridge | doorbell | TCP 9000 |
| bridge | console | TCP 7442 (camera protocol), 443 (login), 7444 (snapshot upload), 7550 (video) |
| console | bridge | UDP 7004 (talkback) |
| you | bridge | TCP 8080 (web page) |

## How it works

- **Doorbell side.** Two Baichuan streams (main, sub) feed Protect's video
  channels. A third Baichuan connection subscribes to push events (motion, AI
  type, button). Settings are short GET → modify → SET exchanges that keep every
  element the bridge does not change. The doorbell's AI boxes travel inside the
  video frames (compressed TLV records in the frame header) and are decoded from
  the main stream.
- **Protect side.** The bridge connects to `wss://<console>:7442` as a camera,
  identifies itself with a G4 Doorbell's system id and firmware version, and
  pushes each video channel to Protect as UniFi's FLV variant. It follows the
  firmware's own writer (`libubnt_encoder`): 64-bit clock trailers, `onMpma` and
  `onClockSync` before every keyframe, the camera's `onMetaData`, and frame-rate
  timing in the H.264 SPS.
- **Events** are sent as a camera sends them: `EventAnalytics` for motion,
  `EventSmartDetect` with descriptors, zone and line status for detections, and
  `MCUEventMessage` / `EventRingButtonPressed` for the ring, which Protect's `ds`
  service turns into a ring event. A fallback rings through Protect's HTTP API
  (Options → ring method).
- **Talkback**: Protect sends ADTS AAC (22.05 kHz) over UDP. ffmpeg decodes it,
  and the PCM goes to the doorbell's talk channel (ADPCM).

## Development

```sh
go test -race ./...
go build ./cmd/reolink-protect-bridge
```

The code layout:

| Path | Contents |
|---|---|
| `cmd/reolink-protect-bridge` | main |
| `internal/bridge` | one doorbell as a Protect camera: streams, events, talkback, settings, box geometry |
| `internal/source` | keeps a Reolink stream connected and re-attaches consumers after reconnects |
| `internal/web`, `internal/config`, `internal/discovery`, `internal/logbuf` | web page and API, config store, discovery, log buffer |
| `pkg/unifi` | UniFi camera protocol: adoption, FLV, events, settings, talkback |
| `pkg/baichuan`, `pkg/reolink` | Reolink Baichuan protocol and stream producer |

The media primitives (H.264/AAC parsing, RTP tracks, FLV and AMF) come from
[go2rtc](https://github.com/AlexxIT/go2rtc) as an ordinary Go module dependency
(`github.com/AlexxIT/go2rtc`, public `pkg/` packages only), so it can be updated
independently with `go get github.com/AlexxIT/go2rtc@<version>`.

## Credits and sources

This project builds on the work of others:

- **[go2rtc](https://github.com/AlexxIT/go2rtc)** by Alexey Khit (MIT): the media
  library this app depends on.
- **[fzurita/go2rtc](https://github.com/fzurita/go2rtc)** by Francisco Zurita
  (MIT): the Baichuan client and Reolink producer in `pkg/baichuan` and
  `pkg/reolink`, carried over from his go2rtc fork and extended here (settings
  commands, push events, AI boxes).
- **[unifi-cam-proxy](https://github.com/keshavdv/unifi-cam-proxy)** by Keshav
  Varma (MIT), and its Reolink fork
  **[woutersteegers/reolink-unifi-protect](https://github.com/woutersteegers/reolink-unifi-protect)**:
  the reference for the camera protocol as Protect 7.x accepts it, event
  payloads, the AI box format (via
  [nodelink-js](https://github.com/nodelink-js)) and many hard-won notes.
- **[reolink_aio](https://github.com/starkillerOG/reolink_aio)** by starkillerOG
  (MIT): reference for Baichuan commands, push events and settings.
- **[pyunifiwire](https://github.com/rjmotion/pyunifiwire)** /
  [unifi-guides](https://github.com/rjmotion/unifi-guides) (MIT): measurements of
  the UniFi wire protocol on real hardware.
- Static analysis of publicly downloadable firmware and software: UniFi G4/G5
  camera firmware 5.5.45 (`ubnt_avclient`, `ubnt_ctlserver`, `libubnt_encoder`,
  `ubnt_talkback`), UniFi Protect 7.3.74 and its `ds` 7.3.6 package, and the
  Reolink D340W firmware (`netserver`, `libnetpublic`, `device`). None of it is
  redistributed here.

See [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) for the licence texts.

## License

[MIT](LICENSE)
