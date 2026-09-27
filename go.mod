module github.com/tigersoldier/pi-chat

go 1.26.4

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/coder/websocket v1.8.15
	github.com/tigersoldier/pi-gateway v0.1.2
)

// To develop against a local pi-gateway checkout instead of the published tag
// (gwclient and pi-gatewayd must speak the same protocol version):
//
//	replace github.com/tigersoldier/pi-gateway => ~/code/pi-gateway
