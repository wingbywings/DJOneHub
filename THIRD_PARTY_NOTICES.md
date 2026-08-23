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
