# Reolink Protect Bridge

Shows a Reolink video doorbell in UniFi Protect as a native G4 Doorbell, with
live view, recording, ring, talkback and smart detections. See the
[project README](https://github.com/jgerdes5/reolink-protect-bridge) for
details.

## Setup

1. Start the add-on and open **Reolink Protect** in the sidebar.
2. Under **Connections**, enter the doorbell and your UniFi console. The
   console needs a local UniFi OS user without SSO or 2FA and with full
   Protect access, or an adoption token.
3. Save. The camera appears in Protect and adopts itself.

Home Assistant's UniFi Protect integration then shows it as a doorbell too,
with ring and detection events for automations.

## Network

- The camera uses this Home Assistant host's IP address in Protect. Give the
  host a fixed IP address or a DHCP reservation.
- Protect sends talkback audio to UDP port 7004 on that address, so only one
  doorbell per Home Assistant host is possible.
- The host must reach the doorbell on TCP 9000 and the console on TCP 443,
  7442, 7444 and 7550.

## Notes

- For a privacy mode, draw an exclusion zone over the whole picture in Protect
  and tick **Privacy mask**. The microphone keeps recording.
- The bridge sets the doorbell's keyframe interval to one second, even with
  "Apply camera settings from Protect" turned off.
- The console's certificate is trusted on first use. After replacing it, trust
  the new one on the **Connections** page.

## Data

Configuration, credentials and the camera identity are stored in the add-on's
data folder and included in Home Assistant backups. Restoring a backup keeps
the same camera in Protect.
