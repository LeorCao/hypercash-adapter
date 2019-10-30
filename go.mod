module github.com/LeorCao/hypercash-adapter

go 1.12

require (
	github.com/HcashOrg/bliss v0.0.0-20180719035130-f5d53c2a9b7d // indirect
	github.com/HcashOrg/hcd v0.0.0-20190611115530-593f7eea8b25
	github.com/agl/ed25519 v0.0.0-20170116200512-5312a6153412 // indirect
	github.com/asdine/storm v2.1.2+incompatible
	github.com/astaxie/beego v1.11.1
	github.com/blocktree/go-owcdrivers v1.1.16
	github.com/blocktree/go-owcrypt v1.0.3
	github.com/blocktree/openwallet v1.4.8
	github.com/bndr/gotabulate v1.1.2
	github.com/codeskyblue/go-sh v0.0.0-20190328095946-f4ce45e7999e
	github.com/dchest/blake256 v1.0.0 // indirect
	github.com/ethereum/go-ethereum v1.8.25
	github.com/graarh/golang-socketio v0.0.0-20170510162725-2c44953b9b5f
	github.com/imroc/req v0.2.3
	github.com/pborman/uuid v1.2.0
	github.com/shopspring/decimal v0.0.0-20180709203117-cd690d0c9e24
	github.com/tidwall/gjson v1.2.1
	go.etcd.io/bbolt v1.3.3 // indirect
)

//replace github.com/blocktree/go-owcdrivers => ../../go-owcdrivers
