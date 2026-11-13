# Reolink Protect Bridge

Shows a Reolink video doorbell in UniFi Protect as a native UniFi doorbell:
live view, recording, ring with the call screen, talkback, person/vehicle/
animal detections and settings. See the project README for details.

## Setup

1. Start the add-on and open **Reolink Protect** in the sidebar (Home
   Assistant handles the login).
2. **Connections**: the doorbell (address or *Find doorbells*, user, password)
   and the UniFi console (address or *Find Protect console*, a local UniFi OS
   user without SSO/MFA with full Protect management, or an adoption token).
3. Save. The camera appears in UniFi Protect and adopts itself.

Your Home Assistant installation's existing UniFi Protect integration then
shows it as a doorbell too, with ring and detection events for automations.

## Network

- The camera gets **this Home Assistant host's IP address** in Protect. It must
  not change: give Home Assistant a fixed IP or DHCP reservation.
- Protect sends talkback audio to UDP port **7004** on that address; nothing
  else on the host may use it. One doorbell per Home Assistant host.
- The host must reach the doorbell on TCP 9000 and the console on TCP 7442,
  443, 7444 and 7550.

## Notes

- A privacy mask over the whole picture in Protect blacks out the video only;
  the doorbell's microphone audio still reaches Protect. Ring button and
  detections under a full mask haven't been tested yet.
- On every start the bridge sets the doorbell's keyframe interval to 1 second
  (or as close as the doorbell allows), also with "Apply camera settings from
  Protect" turned off.
- If you change the doorbell or console address, enter its password (and
  adoption token) again.
- The console's certificate is trusted on the first login; the Protect
  password is only sent to a console with the same certificate key. After
  replacing the console's certificate, tick "Trust the certificate the console
  shows next" on the Connections page.

## Data

Configuration, credentials and the camera's identity are stored in the
add-on's data folder and included in Home Assistant backups. Restoring a
backup keeps the same camera in Protect.
