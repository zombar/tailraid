//go:build !darwin && !linux && !windows

package admin

func platformEncryption(string) Encryption { return EncryptedUnknown }
