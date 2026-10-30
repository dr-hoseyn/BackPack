module github.com/backpack/backpack

go 1.26.10

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/amnezia-vpn/amneziawg-go/v3 v3.1.20260828
	github.com/cilium/ebpf v0.22.0
	github.com/flynn/noise v1.1.0
	github.com/gorilla/websocket v1.5.3
	github.com/klauspost/reedsolomon v1.14.2
	github.com/quic-go/connect-ip-go v0.6.0
	github.com/quic-go/masque-go v0.6.0
	github.com/quic-go/quic-go v0.64.1
	github.com/quic-go/webtransport-go v0.13.0
	github.com/refraction-networking/utls v1.8.2
	github.com/shirou/gopsutil/v4 v4.26.9
	github.com/sirupsen/logrus v1.10.2
	github.com/xtaci/kcp-go/v5 v5.6.73
	github.com/xtaci/smux v1.5.57
	github.com/yosida95/uritemplate/v3 v3.0.2
	golang.org/x/crypto v0.58.0
	golang.org/x/net v0.61.0
	golang.org/x/sys v0.49.0
	golang.org/x/time v0.16.0
	golang.zx2c4.com/wireguard v0.0.0-20250521234502-f333402bd9cb
)

require (
	github.com/andybalholm/brotli v1.0.6 // indirect
	github.com/dunglas/httpsfv v1.1.1 // indirect
	github.com/ebitengine/purego v0.11.1 // indirect
	github.com/go-ole/go-ole v1.2.6 // indirect
	github.com/klauspost/compress v1.18.7 // indirect
	github.com/klauspost/cpuid/v2 v2.3.0 // indirect
	github.com/lufia/plan9stats v0.0.0-20211012122336-39d0f177ccd0 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/power-devops/perfstat v0.0.0-20260805114148-88456608a4f6 // indirect
	github.com/quic-go/qpack v0.6.0 // indirect
	github.com/tjfoc/gmsm v1.4.1 // indirect
	github.com/tklauser/go-sysconf v0.4.0 // indirect
	github.com/tklauser/numcpus v0.12.0 // indirect
	github.com/yusufpapurcu/wmi v1.2.4 // indirect
	golang.org/x/text v0.43.0 // indirect
	golang.zx2c4.com/wintun v0.0.0-20230126152724-0fa3db229ce2 // indirect
)

replace github.com/xtaci/smux => ./third_party/smux
