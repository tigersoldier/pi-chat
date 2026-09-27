module github.com/tigersoldier/pi-chat

go 1.26.4

require github.com/BurntSushi/toml v1.6.0

// pi-gateway is required once gwclient is imported (PLAN.md M3). To develop
// against a local checkout instead of the published module:
//
//	replace github.com/tigersoldier/pi-gateway => ~/code/pi-gateway
