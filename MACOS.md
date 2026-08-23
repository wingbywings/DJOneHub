# DJOneHub for macOS

This branch adds a native macOS service for the DJI Cellular Dongle / Quectel
EG25-G. It does not require UTM for AT-mode management.

## Current scope

- Automatic discovery of DJI (`2ca3`) and Quectel (`2c7c`) USB serial ports
- Modem, SIM, operator, registration and signal status
- Receive and send SMS through the modem AT port
- Execute explicit AT commands
- Read and switch physical eUICC profiles through AT APDU transport
- Local management page at `http://127.0.0.1:7575`
- Device-specific voice calls with D4/UAC routing and a native CoreAudio host
- Exact UAC binding by USB vendor/product/location ID, microphone mute and WAV recording
- Packaged Apple Silicon release (Intel packaging is planned separately)

The cellular data interface remains managed by macOS. This allows macOS to use
the dongle as its network connection while DJOneHub uses a separate USB serial
interface for management.

## Downloaded release

The Apple Silicon ZIP contains the executable, its libusb runtime, licenses and
the `djonehub` terminal launcher. It does not require Go, Homebrew or a separately
installed libusb on the user's Mac.

From the extracted release directory:

```sh
./djonehub start
```

The terminal remains attached to the service and the management page opens
automatically. Press `Control+C` to stop it, or run `./djonehub stop` from another
terminal in the same directory. Logs are stored in
`~/Library/Logs/DJOneHub/djonehub.log`.

## Build from source

Requirements:

- macOS 13 or newer
- Go 1.26 or newer
- Swift 6 / Xcode command-line tools

```sh
./scripts/package-macos-arm64.sh v0.1.0-preview
```

Release outputs:

- `dist/release/DJOneHub-macOS-arm64-v0.1.0-preview/`
- `dist/release/DJOneHub-macOS-arm64-v0.1.0-preview.zip`
- `dist/release/DJOneHub-macOS-arm64-v0.1.0-preview.zip.sha256`

The packaging script downloads the official libusb source archive, verifies its
SHA-256, builds it for macOS 13 or newer, builds `DJOneHubAudioHost`, and bundles
both executables. In normal mode the launcher starts the audio host automatically;
demo mode never requests microphone access.

The first real call requires microphone permission. Module USB/IMS/VoLTE
initialization is never automatic: the Calls page requires explicit confirmation,
saves a device-specific rollback backup, verifies every write, and restores the
original settings if validation fails. The external module voice runtime is also
installed only after a separate confirmation and pinned SHA-256 verification.

Baiwang QDC507GLEFM21 firmware may retain the legacy `1,1,1,1,1,0,1` layout
until ADB is authorized with a device-specific QADBKEY passcode. DJOneHub reports
that layout as requiring ADB unlock instead of treating USB Audio alone as full
call readiness. The Calls page accepts the official passcode over the local API,
submits it through a non-logging USB AT path, never persists it, then requires an
exact `1,1,1,1,1,1,1` readback before rebooting the module.

Before dialing or answering, DJOneHub prepares the module runtime and retains the
same ADB transport across `ATD`/`ATA`. The D4/UAC media route is still activated
only after `CLCC` reports an active call. This avoids QDC507 firmware that rejects
a new ADB handshake once a voice call is active.

## Run

Connect the modem and run:

```sh
./dist/djonehub-macos
```

If automatic discovery picks no AT port, inspect `/dev/cu.*` and pass it:

```sh
./dist/djonehub-macos -port /dev/cu.usbmodemXXXX
```

The server only listens on localhost by default. Open:

```text
http://127.0.0.1:7575
```

## Demo without hardware

To explore the management page before buying the module, run:

```sh
./dist/djonehub-macos -demo
```

Then open `http://127.0.0.1:7575`. Demo mode provides simulated modem status,
SMS messages, AT command responses and eSIM profiles. It does not access a real
SIM, send messages or switch a physical eSIM profile.

## Launch at login

```sh
./scripts/install-macos.sh
```

Logs are written to `~/Library/Logs/DJOneHub`.

## Platform limitations

- Native QMI/MBIM control, Linux udev and network-namespace orchestration are
  excluded from this macOS entry point.
- eSIM behavior depends on the physical eUICC and modem firmware. Profile
  switching must be verified with real hardware.
- The release uses an ad-hoc signature rather than an Apple Developer ID. On
  first run, macOS may require approval in Privacy & Security.
- Voice calling remains experimental until inbound/outbound audio, active-device
  removal and multi-module isolation have passed real-hardware acceptance.
