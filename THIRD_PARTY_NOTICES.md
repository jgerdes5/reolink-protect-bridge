# Third-party notices

## go2rtc and fzurita/go2rtc (pkg/baichuan, pkg/reolink; dependency github.com/AlexxIT/go2rtc)

The packages in `pkg/baichuan` and `pkg/reolink` were written by Francisco Zurita in his fork of go2rtc (github.com/fzurita/go2rtc) and are distributed under go2rtc's licence:

```
MIT License

Copyright (c) 2022 Alexey Khit

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

## unifi-cam-proxy (protocol reference; no code copied)

```
The MIT License

Copyright (c) 2023 Keshav Varma
```

The Go code in `pkg/unifi` re-implements the camera protocol as unifi-cam-proxy and its Reolink fork (woutersteegers/reolink-unifi-protect) use it; both are MIT-licensed (Copyright (c) 2023 Keshav Varma). The AI box decoder in `pkg/baichuan/aibox.go` follows the format decoded by that fork's sidecar.

## reolink_aio, pyunifiwire (protocol references; no code copied)

reolink_aio: MIT, Copyright (c) 2023 starkillerOG. pyunifiwire: MIT, Copyright (c) 2026 flock contributors.
