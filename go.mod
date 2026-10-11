module github.com/zombar/tailraid

go 1.27.2

require (
	github.com/cloudsoda/go-smb2 v0.0.0-20260918041005-0c5d69b69701
	github.com/dhowden/tag v0.0.0-20240417053706-3d75831295e8
	github.com/go-filesystems/interface v0.5.0
	github.com/go-filesystems/smb v0.7.0
	github.com/go-git/go-billy/v5 v5.9.2
	github.com/goccy/go-yaml v1.19.2
	github.com/hashicorp/raft v1.8.0
	github.com/hashicorp/raft-boltdb/v2 v2.4.2
	github.com/klauspost/reedsolomon v1.14.2
	github.com/spf13/cobra v1.10.2
	github.com/willscott/go-nfs v0.0.4
	github.com/willscott/go-nfs-client v0.0.0-20251022144359-801f10d98886
	go.etcd.io/bbolt v1.5.0
	go.uber.org/goleak v1.3.0
	golang.org/x/crypto v0.57.0
	golang.org/x/image v0.47.0
	golang.org/x/sys v0.49.0
	gopkg.in/check.v1 v1.0.0-20201130134442-10cb98267c6c
)

require (
	github.com/boltdb/bolt v1.3.1 // indirect
	github.com/cloudsoda/sddl v0.0.0-20250224235906-926454e91efc // indirect
	github.com/fatih/color v1.19.0 // indirect
	github.com/geoffgarside/ber v1.1.0 // indirect
	github.com/go-filesystems/hostcopy v0.1.0 // indirect
	github.com/hashicorp/go-hclog v1.6.3 // indirect
	github.com/hashicorp/go-immutable-radix v1.3.1 // indirect
	github.com/hashicorp/go-metrics v0.7.0 // indirect
	github.com/hashicorp/go-msgpack/v2 v2.1.5 // indirect
	github.com/hashicorp/go-uuid v1.0.3 // indirect
	github.com/hashicorp/golang-lru v1.0.2 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/jcmturner/aescts/v2 v2.0.0 // indirect
	github.com/jcmturner/dnsutils/v2 v2.0.0 // indirect
	github.com/jcmturner/gofork v1.7.6 // indirect
	github.com/jcmturner/goidentity/v6 v6.0.1 // indirect
	github.com/jcmturner/gokrb5/v8 v8.4.4 // indirect
	github.com/jcmturner/rpc/v2 v2.0.3 // indirect
	github.com/klauspost/cpuid/v2 v2.3.0 // indirect
	github.com/kr/pretty v0.3.1 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/mattn/go-colorable v0.1.15 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/rasky/go-xdr v0.0.0-20170124162913-1a41d1a06c93 // indirect
	github.com/rogpeppe/go-internal v1.14.1 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	golang.org/x/net v0.58.0 // indirect
)

replace github.com/go-filesystems/smb => ./third_party/smb
