// Package wintun carries the signed wintun.dll that the Windows device needs beside the
// tailraid binary (docs/20 §9, I-324): `install` and `update` write it there. The
// prebuilt binaries are redistributed under WireGuard LLC's prebuilt-binaries licence
// (LICENSE.txt here): tailraid uses them only through the permitted API, by way of
// wireguard-go's wintun package. Off Windows the package does nothing.
package wintun

// Version is the wintun release the embedded binaries come from (wintun.net/builds).
const Version = "0.14.1"

// Name is the file written beside the binary.
const Name = "wintun.dll"
