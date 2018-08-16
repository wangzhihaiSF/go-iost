package global

import (
	"github.com/iost-official/Go-IOS-Protocol/common"
	"github.com/iost-official/Go-IOS-Protocol/core/new_block"
	"github.com/iost-official/Go-IOS-Protocol/core/new_tx"
	"github.com/iost-official/Go-IOS-Protocol/db"
)

//go:generate mockgen -destination ../mocks/mock_global.go -package core_mock github.com/iost-official/Go-IOS-Protocol/core/global BaseVariable

type BaseVariable interface {
	TxDB() tx.TxDB
	StateDB() *db.MVCCDB
	Config() *common.Config
	BlockChain() block.Chain
	Mode() *Mode
}