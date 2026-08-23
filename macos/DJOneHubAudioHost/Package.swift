// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "DJOneHubAudioHost",
    platforms: [.macOS(.v13)],
    products: [
        .executable(name: "DJOneHubAudioHost", targets: ["DJOneHubAudioHost"]),
    ],
    targets: [
        .target(
            name: "CModemBridge",
            path: "Sources/CModemBridge",
            publicHeadersPath: "include",
            linkerSettings: [
                .linkedFramework("CoreFoundation"),
                .linkedFramework("IOKit"),
            ]
        ),
        .target(
            name: "CUACProbe",
            path: "Sources/CUACProbe",
            publicHeadersPath: "include",
            linkerSettings: [
                .linkedFramework("CoreAudio"),
                .linkedFramework("CoreFoundation"),
                .linkedFramework("IOKit"),
            ]
        ),
        .executableTarget(
            name: "DJOneHubAudioHost",
            dependencies: ["CModemBridge", "CUACProbe"],
            path: "Sources/DJOneHubAudioHost",
            swiftSettings: [.swiftLanguageMode(.v5)],
            linkerSettings: [.linkedFramework("AVFoundation")]
        ),
    ]
)
