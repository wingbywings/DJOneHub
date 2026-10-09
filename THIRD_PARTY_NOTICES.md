# Third-Party Notices

DJOneHub contains code derived from the upstream VoHive project and retains the license and required notice provided in the repository root [`LICENSE`](LICENSE):

```text
Required Notice: Copyright iniwex5 (https://github.com/iniwex5/vohive)
```

## Release Runtime

The macOS release package includes **libusb 1.0.30**, distributed under the GNU Lesser General Public License, version 2.1 or later.

- Project: <https://libusb.info/>
- Source: <https://github.com/libusb/libusb/releases/tag/v1.0.30>
- License text in the release package: `licenses/libusb-COPYING`

## Vendored Source Dependencies

The source repository includes vendored dependencies under `third_party/` so the versions used by DJOneHub remain reproducible. Their original copyright notices and license texts are retained in the corresponding directories.

| Component | License file |
| --- | --- |
| euicc-go | `third_party/euicc-go/LICENSE` |
| uicc-go | `third_party/uicc-go/LICENSE` |
| quectel-qmi-go | `third_party/quectel-qmi-go/LICENSE` |
| strftime | `third_party/strftime/LICENSE` |
| pkg/errors | `third_party/pkg-errors/LICENSE` |
| golang.org/x/sys | `third_party/x-sys/LICENSE` |
| golang.org/x/text | `third_party/x-text/LICENSE` |
| multierr | `third_party/multierr/LICENSE.txt` |

Dependencies fetched through Go modules retain their own licenses and copyright notices. This file is informational and does not replace any component's full license text.

## MaVo macOS voice audio

The macOS `DJOneHubAudioHost` contains host-side USB voice and CoreAudio code
derived from MaVo, licensed under the MIT License. Source comments and SPDX
identifiers from the reference implementation are retained under
`macos/DJOneHubAudioHost/Sources/`.

- Project: <https://github.com/moluncn/mavo>
- Pinned module runtime source: commit `0443dfdaf8aec086fd76ba2ee9152fd908114524`
- Module-side binary files are not included in this repository or release;
  they are downloaded only after explicit confirmation and SHA-256 verification.

## Optional macOS Opus codec

Remote-media development builds link the locally installed libopus through
pkg-config. The supported versions are 1.5.2 and 1.6.1. Opus uses a BSD license;
its source distribution also retains the applicable patent notices.

- Project and releases: <https://opus-codec.org/downloads/>
- License and patent notices: <https://opus-codec.org/license/>
- This build-script integration does not bundle libopus in the macOS release ZIP.
  A distribution that includes the library must retain its required notices.

## iOS WebRTC

The iOS online-call development app links WebRTC M154 through the `stasel/WebRTC`
Swift package, pinned to version `154.0.0`, revision
`0c0ad84dac6c1941c16a414dfcfba94691866e4c` in the Xcode project's `Package.resolved`.
The package is a third-party binary distribution, not an Apple or DJOneHub build.
Its package manifest pins the XCFramework archive SHA-256 to
`a2bcdda93578c82452ceb6e49d54a2746e1bcb4caf7c2fa601ffac8028b58c16`.

- Upstream project: <https://webrtc.googlesource.com/src/>
- Binary distribution: <https://github.com/stasel/WebRTC/tree/154.0.0>
- The archive's WebRTC BSD license is retained and bundled in
  `ios/DJOneHubPhone/Resources/WebRTC-LICENSE.txt`.
- Release preparation must also account for the notices of dependencies included
  in the selected binary distribution; this development entry does not replace
  the release license inventory.

The TURN integration uses Pion TURN v5.1.2 in local relay tests and follows
coturn's time-limited REST credential format. Coturn is deployed separately and
is not bundled in the repository or application.
