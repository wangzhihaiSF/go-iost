package new_vm

import (
	"sync"

	"errors"

	"runtime"

	"github.com/bitly/go-simplejson"
	"github.com/iost-official/Go-IOS-Protocol/account"
	"github.com/iost-official/Go-IOS-Protocol/core/contract"
	"github.com/iost-official/Go-IOS-Protocol/core/new_block"
	"github.com/iost-official/Go-IOS-Protocol/core/new_tx"
	"github.com/iost-official/Go-IOS-Protocol/ilog"
	"github.com/iost-official/Go-IOS-Protocol/new_vm/database"
	"github.com/iost-official/Go-IOS-Protocol/new_vm/host"
)

const (
	defaultCacheLength = 1000
)

const (
	GasCheckTxFailed = int64(100)
)

var (
	ErrContractNotFound = errors.New("contract not found")

	ErrSetUpArgs = errors.New("key does not exist")
)

type Engine interface {
	SetUp(k, v string) error
	Exec(tx0 *tx.Tx) (*tx.TxReceipt, error)
	GC()
}

var staticMonitor *Monitor

var once sync.Once

type EngineImpl struct {
	ho *host.Host

	jsPath string

	logger        *ilog.Logger
	consoleWriter *ilog.ConsoleWriter
	fileWriter    *ilog.FileWriter
}

func NewEngine(bh *block.BlockHead, cb database.IMultiValue) Engine {
	if staticMonitor == nil {
		once.Do(func() {
			staticMonitor = NewMonitor()
		})
	}

	ctx := host.NewContext(nil)

	ctx = loadBlkInfo(ctx, bh)

	db := database.NewVisitor(defaultCacheLength, cb)

	logger := ilog.New()
	logger.Stop()
	h := host.NewHost(ctx, db, staticMonitor, logger)

	e := &EngineImpl{ho: h, logger: logger}
	runtime.SetFinalizer(e, func(e *EngineImpl) {
		e.GC()
	})

	return e
}

/*
SetUp keys:
	js_path   	path to libjs/
	log_level 	log level of this engine(debug, info, warning, error, fatal), unset to silent log
	log_path	path to log file, unset to disable saving logs
	log_enable	enable log, log_level should set
*/
func (e *EngineImpl) SetUp(k, v string) error {
	switch k {
	case "js_path":
		e.jsPath = v
	case "log_level":
		e.setLogger(v, "", false)
	case "log_path":
		e.setLogger("", v, false)
	case "log_enable":
		e.setLogger("", "", true)
	default:
		return ErrSetUpArgs
	}
	return nil
}
func (e *EngineImpl) Exec(tx0 *tx.Tx) (*tx.TxReceipt, error) {
	err := checkTx(tx0)
	if err != nil {
		return errReceipt(tx0.Hash(), tx.ErrorTxFormat, err.Error()), nil
	}

	bl := e.ho.DB.Balance(account.GetIdByPubkey(tx0.Publisher.Pubkey))
	if bl <= 0 || bl < tx0.GasPrice*tx0.GasLimit {
		return errReceipt(tx0.Hash(), tx.ErrorBalanceNotEnough, "publisher's balance less than price * limit"), nil
	}

	e.ho.Ctx = loadTxInfo(e.ho.Ctx, tx0)

	e.ho.Ctx.GSet("gas_limit", tx0.GasLimit)
	e.ho.Ctx.GSet("receipts", make([]tx.Receipt, 0))

	txr := tx.NewTxReceipt(tx0.Hash())

	for _, action := range tx0.Actions {

		cost, status, receipts, err := e.runAction(action)
		if err != nil {
			return nil, err
		}

		if cost == nil {
			panic("cost is nil")
		}

		txr.Status = status
		txr.GasUsage += cost.ToGas()

		if status.Code != tx.Success {
			txr.Receipts = nil
			e.ho.DB.Rollback()
			break
		}

		txr.Receipts = append(txr.Receipts, receipts...)
		txr.SuccActionNum++

		gasLimit := e.ho.Ctx.GValue("gas_limit").(int64)
		e.ho.Ctx.GSet("gas_limit", gasLimit-cost.ToGas())

		e.ho.PayCost(cost, account.GetIdByPubkey(tx0.Publisher.Pubkey))
	}

	e.ho.DoPay(e.ho.Ctx.Value("witness").(string), int64(tx0.GasPrice))
	e.ho.DB.Commit()

	return &txr, nil
}
func (e *EngineImpl) GC() {
	e.logger.Stop()
}

func checkTx(tx0 *tx.Tx) error {
	if tx0.GasPrice <= 0 || tx0.GasPrice > 10000 {
		return ErrGasPriceTooBig
	}
	return nil
}
func unmarshalArgs(abi *contract.ABI, data string) ([]interface{}, error) {
	js, err := simplejson.NewJson([]byte(data))
	if err != nil {
		return nil, err
	}

	rtn := make([]interface{}, 0)
	arr, err := js.Array()
	if err != nil {
		return nil, err
	}

	if len(arr) < len(abi.Args) {
		panic("less args ")
	}
	for i := range arr {
		switch abi.Args[i] {
		case "string":
			s, err := js.GetIndex(i).String()
			if err != nil {
				return nil, err
			}
			rtn = append(rtn, s)
		case "bool":
			s, err := js.GetIndex(i).Bool()
			if err != nil {
				return nil, err
			}
			rtn = append(rtn, s)
		case "number":
			s, err := js.GetIndex(i).Int64()
			if err != nil {
				return nil, err
			}
			rtn = append(rtn, s)
		case "json":
			s, err := js.GetIndex(i).Encode()
			if err != nil {
				return nil, err
			}
			rtn = append(rtn, database.SerializedJSON(s))
		}
	}

	return rtn, nil
	//return nil, errors.New("unsupported yet")

}
func errReceipt(hash []byte, code tx.StatusCode, message string) *tx.TxReceipt {
	return &tx.TxReceipt{
		TxHash:   hash,
		GasUsage: GasCheckTxFailed,
		Status: tx.Status{
			Code:    code,
			Message: message,
		},
		SuccActionNum: 0,
		Receipts:      make([]tx.Receipt, 0),
	}
}
func (e *EngineImpl) runAction(action tx.Action) (cost *contract.Cost, status tx.Status, receipts []tx.Receipt, err error) {
	receipts = make([]tx.Receipt, 0)
	cost = contract.Cost0()

	e.ho.Ctx = host.NewContext(e.ho.Ctx)
	defer func() {
		e.ho.Ctx = e.ho.Ctx.Base()
	}()

	e.ho.Ctx.Set("stack0", "direct_call")
	e.ho.Ctx.Set("stack_height", 1) // record stack trace

	c := e.ho.DB.Contract(action.Contract)
	if c.Info == nil {
		cost = contract.NewCost(0, 0, GasCheckTxFailed)
		status = tx.Status{
			Code:    tx.ErrorParamter,
			Message: ErrContractNotFound.Error() + action.Contract,
		}
		return
	}

	abi := c.ABI(action.ActionName)
	if abi == nil {
		cost = contract.NewCost(0, 0, GasCheckTxFailed)
		status = tx.Status{
			Code:    tx.ErrorParamter,
			Message: ErrABINotFound.Error() + action.Contract,
		}
		return
	}

	args, err := unmarshalArgs(abi, action.Data)
	if err != nil {
		cost = contract.NewCost(0, 0, GasCheckTxFailed)
		status = tx.Status{
			Code:    tx.ErrorParamter,
			Message: err.Error(),
		}
		return
	}

	_, cost, err = staticMonitor.Call(e.ho, action.Contract, action.ActionName, args...)

	if cost == nil {
		cost = contract.Cost0()
	}

	if err != nil {
		status = tx.Status{
			Code:    tx.ErrorRuntime,
			Message: err.Error(),
		}
		receipt := tx.Receipt{
			Type:    tx.SystemDefined,
			Content: err.Error(),
		}
		receipts = append(receipts, receipt)

		err = nil

		return
	}

	receipts = append(receipts, e.ho.Ctx.GValue("receipts").([]tx.Receipt)...)

	status = tx.Status{
		Code:    tx.Success,
		Message: "",
	}
	return
}

func (e *EngineImpl) setLogger(level, path string, start bool) {

	if path == "" {

		if e.consoleWriter == nil {
			e.consoleWriter = ilog.NewConsoleWriter()
		}

		switch level {
		case "debug":
			e.consoleWriter.SetLevel(ilog.LevelDebug)
		case "info":
			e.consoleWriter.SetLevel(ilog.LevelInfo)
		case "warning":
			e.consoleWriter.SetLevel(ilog.LevelWarn)
		case "error":
			e.consoleWriter.SetLevel(ilog.LevelError)
		case "fatal":
			e.consoleWriter.SetLevel(ilog.LevelFatal)
		}

		return
	}

	if level == "" {
		e.fileWriter = ilog.NewFileWriter(path)
	}

	if start {
		var ok bool
		if e.consoleWriter != nil {
			e.logger.AddWriter(e.consoleWriter)
			ok = true
		}
		if e.fileWriter != nil {
			e.logger.AddWriter(e.fileWriter)
			ok = true
		}

		if ok {
			e.logger.Start()
		}
	}
}

func loadBlkInfo(ctx *host.Context, bh *block.BlockHead) *host.Context {
	c := host.NewContext(ctx)
	c.Set("parent_hash", bh.ParentHash)
	c.Set("number", bh.Number)
	c.Set("witness", bh.Witness)
	c.Set("time", bh.Time)
	return c
}

func loadTxInfo(ctx *host.Context, t *tx.Tx) *host.Context {
	c := host.NewContext(ctx)
	c.Set("time", t.Time)
	c.Set("expiration", t.Expiration)
	c.Set("gas_price", t.GasPrice)

	authList := make(map[string]int)
	for _, v := range t.Signers {
		authList[string(v)] = 1
	}

	authList[account.GetIdByPubkey(t.Publisher.Pubkey)] = 2

	c.Set("auth_list", authList)

	return c
}