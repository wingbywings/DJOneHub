// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "DJOneHubIdentity",
    platforms: [.macOS(.v13), .iOS(.v17)],
    products: [.library(name: "DJOneHubIdentity", targets: ["DJOneHubIdentity"])],
    targets: [.target(name: "DJOneHubIdentity"), .testTarget(name: "DJOneHubIdentityTests", dependencies: ["DJOneHubIdentity"])]
)
