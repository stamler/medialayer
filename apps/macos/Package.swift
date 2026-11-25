// swift-tools-version:5.10
//
// The MediaLayer Mac player, proof of concept. Build it with
// scripts/build-mac-app.sh from the repository root: that builds the Go
// core into build/libmlcore.a first, then this package, then wraps the
// binary in MediaLayer.app.
import PackageDescription

let package = Package(
    name: "MediaLayer",
    platforms: [.macOS(.v14)],
    targets: [
        // The Go core's C interface (cmd/mlcore). The header is generated
        // by `go build -buildmode=c-archive` and copied in by the script.
        .target(
            name: "CMLCore",
            path: "Sources/CMLCore",
            linkerSettings: [
                .linkedLibrary("mlcore"), // found with -L build, passed by the script
                .linkedFramework("CoreFoundation"),
                .linkedFramework("Security"),
                .linkedLibrary("resolv"),
            ]
        ),
        .executableTarget(
            name: "MediaLayer",
            dependencies: ["CMLCore"],
            path: "Sources/MediaLayer",
            linkerSettings: [.linkedLibrary("sqlite3")]
        ),
    ]
)
