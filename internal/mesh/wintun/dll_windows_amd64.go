//go:build windows && amd64

package wintun

import _ "embed"

//go:embed amd64/wintun.dll
var dll []byte
