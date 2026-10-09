// swift-tools-version: 6.0
import PackageDescription
let package = Package(name: "DJOneHubPhoneCore", platforms: [.iOS(.v17), .macOS(.v13)],
    products: [.library(name: "DJOneHubPhoneCore", targets: ["DJOneHubPhoneCore"])],
    dependencies: [.package(path: "../DJOneHubIdentity")],
    targets: [.target(name: "DJOneHubPhoneCore", dependencies: ["DJOneHubIdentity"]),
              .testTarget(name: "DJOneHubPhoneCoreTests", dependencies: ["DJOneHubPhoneCore"])],
    swiftLanguageModes: [.v5])
