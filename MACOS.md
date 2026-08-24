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
- Pluggable AI voice calls: Qwen/OpenAI native realtime audio, or Qwen/OpenAI STT with MiniMax LLM/TTS
- Optional delayed auto-answer with a final ringing-state check; disabled by default
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
- Go 1.26.3 or a compatible newer toolchain
- Swift 6 / Xcode command-line tools
- `pkg-config` and libusb for local development builds

Run the test and debug builds from the repository root:

```sh
go test ./...
./scripts/build-macos.sh
swift build --package-path macos/DJOneHubAudioHost -c debug
```

The daily build script builds only the Go service. Voice calls require the Swift
audio host as a second process:

```sh
./dist/djonehub-macos
swift run --package-path macos/DJOneHubAudioHost \
  DJOneHubAudioHost --base-url http://127.0.0.1:7575
```

The release packaging command below builds and bundles both processes:

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

## AI voice agent configuration

Cloud API keys are read only by the Go service. Export the required values before
starting `djonehub-macos`; they are not stored in the browser, Swift host, or the
persisted voice-agent profile.

```sh
# Set only the providers you plan to use.
export DASHSCOPE_API_KEY='...'
export OPENAI_API_KEY='...'
export MINIMAX_API_KEY='...'
```

Inspect provider readiness and enable Qwen native realtime audio:

```sh
curl http://127.0.0.1:7575/api/voice-agent/status
curl -X PUT http://127.0.0.1:7575/api/voice-agent/config \
  -H 'Content-Type: application/json' \
  -d '{"enabled":true,"provider":"qwen","voice":"Cherry","instructions":"Answer calls concisely.","auto_answer":false,"auto_answer_delay_ms":1200}'
```

Supported profiles are `qwen`, `openai`, and `minimax`. The MiniMax cascade needs
`stt_provider` set to `qwen` or `openai`. Model names and service endpoints can
be overridden with environment variables. See
[`docs/voice-agent-first-batch.md`](docs/voice-agent-first-batch.md) for complete
examples and [`BUILD_AND_RUN_CN.md`](BUILD_AND_RUN_CN.md) for the two-process
development workflow.

The Calls page now provides the complete profile editor, provider/STT fallback,
live transcripts, redacted audit history, and operator approval for sensitive
tools. The local media WebSocket reconnects with exponential backoff; a profile
revision change rebuilds the active media session, and server VAD speech-start
events clear queued assistant audio for barge-in. MiniMax text generation uses
SSE sentence streaming so TTS can begin before the full answer completes.

The profile is stored as `voice-agent.json` under DJOneHub's Application Support
directory (per device in multi-device mode), with mode `0600`. Auto-answer is off
by default and rechecks that the same call is still ringing after the configured
delay before issuing `ATA`.

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
- AI voice providers require network access and valid third-party credentials;
  production use still requires weak-network, interruption and long-call testing.
