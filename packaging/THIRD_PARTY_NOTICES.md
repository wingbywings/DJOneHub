# Third-Party Notices

DJOneHub for macOS includes **libusb 1.0.30**, distributed under the GNU Lesser General Public License, version 2.1 or later.

- Project: <https://libusb.info/>
- Source: <https://github.com/libusb/libusb/releases/tag/v1.0.30>
- License text: `licenses/libusb-COPYING`

The remote-enabled backend statically includes **Opus 1.6.1**, distributed under
the BSD-style license and patent notices in its COPYING file.

- Project: <https://opus-codec.org/>
- Source: <https://downloads.xiph.org/releases/opus/opus-1.6.1.tar.gz>
- Official checksums: <https://downloads.xiph.org/releases/opus/SHA256SUMS.txt>
- License text: `licenses/opus-COPYING`

DJOneHub is distributed under the terms in `LICENSE`, including its required upstream notice.

The bundled `DJOneHubAudioHost` contains MIT-licensed host-side USB voice and
CoreAudio code derived from MaVo (<https://github.com/moluncn/mavo>). The
module-side runtime is not bundled; DJOneHub only downloads the pinned upstream
files after explicit confirmation and verifies their SHA-256 hashes.
