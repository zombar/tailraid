//go:build windows && arm64

package wintun

import _ "embed"

//go:embed arm64/wintun.dll
var dll []byte
