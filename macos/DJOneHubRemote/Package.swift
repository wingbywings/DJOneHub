// swift-tools-version: 6.0
import PackageDescription
let package = Package(name: "DJOneHubRemote", platforms: [.macOS(.v13)],
  products: [.executable(name: "DJOneHubRemote", targets: ["DJOneHubRemote"])],
  dependencies: [.package(path: "../../shared/DJOneHubPhoneCore")],
  targets: [
    .target(name: "RemoteManagement", dependencies: [.product(name: "DJOneHubPhoneCore", package: "DJOneHubPhoneCore")]),
    .executableTarget(name: "DJOneHubRemote", dependencies: ["RemoteManagement", .product(name: "DJOneHubPhoneCore", package: "DJOneHubPhoneCore")]),
    .testTarget(name: "RemoteManagementTests", dependencies: ["RemoteManagement"])
  ], swiftLanguageModes: [.v5])
