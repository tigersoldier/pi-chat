module github.com/tigersoldier/pi-gchat/tools/gateway-probe

go 1.22

// Nested module, deliberately outside the parent build. The replace points at
// the local pi-gateway checkout; swap it for the published version once M2
// renames the parent module.
require github.com/tigersoldier/pi-gateway v0.1.2

replace github.com/tigersoldier/pi-gateway => /home/pi/code/pi-gateway
