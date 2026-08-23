# Third-Party Notices

DJOneHub for macOS includes **libusb 1.0.30**, distributed under the GNU Lesser General Public License, version 2.1 or later.

- Project: <https://libusb.info/>
- Source: <https://github.com/libusb/libusb/releases/tag/v1.0.30>
- License text: `licenses/libusb-COPYING`

DJOneHub is distributed under the terms in `LICENSE`, including its required upstream notice.

The bundled `DJOneHubAudioHost` contains MIT-licensed host-side USB voice and
CoreAudio code derived from MaVo (<https://github.com/moluncn/mavo>). The
module-side runtime is not bundled; DJOneHub only downloads the pinned upstream
files after explicit confirmation and verifies their SHA-256 hashes.
