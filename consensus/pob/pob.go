package pob

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"bytes"

	"github.com/iost-official/Go-IOS-Protocol/account"
	"github.com/iost-official/Go-IOS-Protocol/common"
	"github.com/iost-official/Go-IOS-Protocol/consensus/synchronizer"
	"github.com/iost-official/Go-IOS-Protocol/core/block"
	"github.com/iost-official/Go-IOS-Protocol/core/blockcache"
	"github.com/iost-official/Go-IOS-Protocol/core/global"
	"github.com/iost-official/Go-IOS-Protocol/core/message"
	"github.com/iost-official/Go-IOS-Protocol/core/txpool"
	"github.com/iost-official/Go-IOS-Protocol/db"
	"github.com/iost-official/Go-IOS-Protocol/ilog"
	"github.com/iost-official/Go-IOS-Protocol/metrics"
	"github.com/iost-official/Go-IOS-Protocol/p2p"
	"github.com/iost-official/Go-IOS-Protocol/vm"
)

var (
	metricsGeneratedBlockCount = metrics.NewCounter("iost_pob_generated_block", nil)
	metricsConfirmedLength     = metrics.NewGauge("iost_pob_confirmed_length", nil)
	metricsTxSize              = metrics.NewGauge("iost_block_tx_size", nil)
	metricsMode                = metrics.NewGauge("iost_node_mode", nil)
)

var errSingle = errors.New("single block")

var blockReqTimeout = 3 * time.Second

//PoB is a struct that handles the consensus logic.
type PoB struct {
	account         *account.Account
	baseVariable    global.BaseVariable
	blockChain      block.Chain
	blockCache      blockcache.BlockCache
	txPool          txpool.TxPool
	p2pService      p2p.Service
	synchronizer    synchronizer.Synchronizer
	verifyDB        db.MVCCDB
	produceDB       db.MVCCDB
	blockReqMap     *sync.Map
	exitSignal      chan struct{}
	chRecvBlock     chan p2p.IncomingMessage
	chRecvBlockHead chan p2p.IncomingMessage
	chQueryBlock    chan p2p.IncomingMessage
	chGenBlock      chan *block.Block
}

// NewPoB init a new PoB.
func NewPoB(account *account.Account, baseVariable global.BaseVariable, blockCache blockcache.BlockCache, txPool txpool.TxPool, p2pService p2p.Service, synchronizer synchronizer.Synchronizer, witnessList []string) *PoB {
	p := PoB{
		account:         account,
		baseVariable:    baseVariable,
		blockChain:      baseVariable.BlockChain(),
		blockCache:      blockCache,
		txPool:          txPool,
		p2pService:      p2pService,
		synchronizer:    synchronizer,
		verifyDB:        baseVariable.StateDB(),
		produceDB:       baseVariable.StateDB().Fork(),
		blockReqMap:     new(sync.Map),
		exitSignal:      make(chan struct{}),
		chRecvBlock:     p2pService.Register("consensus channel", p2p.NewBlock, p2p.SyncBlockResponse),
		chRecvBlockHead: p2pService.Register("consensus block head", p2p.NewBlockHead),
		chQueryBlock:    p2pService.Register("consensus query block", p2p.NewBlockRequest),
		chGenBlock:      make(chan *block.Block, 10),
	}
	staticProperty = newStaticProperty(p.account, witnessList)
	return &p
}

//Start make the PoB run.
func (p *PoB) Start() error {
	go p.messageLoop()
	go p.blockLoop()
	go p.scheduleLoop()
	return nil
}

//Stop make the PoB stop.
func (p *PoB) Stop() {
	close(p.exitSignal)
	close(p.chRecvBlock)
	close(p.chGenBlock)
}

func (p *PoB) messageLoop() {
	for {
		select {
		case incomingMessage, ok := <-p.chRecvBlockHead:
			if !ok {
				ilog.Infof("chRecvBlockHead has closed")
				return
			}
			var blk block.Block
			err := blk.DecodeHead(incomingMessage.Data())
			if err != nil {
				continue
			}
			go p.handleRecvBlockHead(&blk, incomingMessage.From())
		case incomingMessage, ok := <-p.chQueryBlock:
			if !ok {
				ilog.Infof("chRecvBlockHead has closed")
				return
			}
			var rh message.RequestBlock
			err := rh.Decode(incomingMessage.Data())
			if err != nil {
				continue
			}
			go p.handleBlockQuery(&rh, incomingMessage.From())
		case <-p.exitSignal:
			return
		}
	}
}

func (p *PoB) handleRecvBlockHead(blk *block.Block, peerID p2p.PeerID) {
	_, ok := p.blockReqMap.Load(string(blk.HeadHash()))
	if ok {
		ilog.Info("block in block request map, block hash: ", blk.HeadHash())
		return
	}
	_, err := p.blockCache.Find(blk.HeadHash())
	if err == nil {
		ilog.Debug(errors.New("duplicate block"))
		return
	}
	err = verifyBasics(blk.Head, blk.Sign)
	if err != nil {
		ilog.Debugf("fail to verify blocks, err:%v", err)
		return
	}

	blkReq := &message.RequestBlock{
		BlockHash: []byte(blk.HeadHash()),
	}
	bytes, err := blkReq.Encode()
	if err != nil {
		ilog.Debugf("fail to verify blocks, %v", err)
		return
	}
	p.blockReqMap.Store(string(blk.HeadHash()), time.AfterFunc(blockReqTimeout, func() {
		p.blockReqMap.Delete(string(blk.HeadHash()))
	}))
	p.p2pService.SendToPeer(peerID, bytes, p2p.NewBlockRequest, p2p.UrgentMessage)
	blkByte, err := blk.EncodeHead()
	if err != nil {
		ilog.Error("fail to encode block head")
		return
	}
	p.p2pService.Broadcast(blkByte, p2p.NewBlockHead, p2p.UrgentMessage)
}

func (p *PoB) handleBlockQuery(rh *message.RequestBlock, peerID p2p.PeerID) {
	var b []byte
	var err error
	node, err := p.blockCache.Find(rh.BlockHash)
	if err == nil {
		b, err = node.Block.Encode()
		if err != nil {
			ilog.Errorf("Fail to encode block: %v, err=%v", rh.BlockNumber, err)
			return
		}
		p.p2pService.SendToPeer(peerID, b, p2p.NewBlock, p2p.UrgentMessage)
		return
	}
	ilog.Infof("failed to get block from blockcache. err=%v", err)
	b, err = p.blockChain.GetBlockByteByHash(rh.BlockHash)
	if err != nil {
		ilog.Warnf("failed to get block from blockchain. err=%v", err)
		return
	}
	p.p2pService.SendToPeer(peerID, b, p2p.NewBlock, p2p.UrgentMessage)
}
func (p *PoB) handleGenesisBlock(blk *block.Block) error {
	if blk.Head.Number == 0 && common.Base58Encode(blk.HeadHash()) == p.baseVariable.Config().Genesis.GenesisHash {
		p.blockCache.AddGenesis(blk)
		err := p.blockChain.Push(blk)
		if err != nil {
			return fmt.Errorf("push block in blockChain failed, err: %v", err)
		}
		engine := vm.NewEngine(blk.Head, p.verifyDB)
		txr, err := engine.Exec(blk.Txs[0])
		if err != nil {
			return fmt.Errorf("exec tx failed, err: %v", err)
		}
		if !bytes.Equal(blk.Receipts[0].Encode(), txr.Encode()) {
			return fmt.Errorf("wrong tx receipt")
		}
		p.verifyDB.Tag(string(blk.HeadHash()))
		err = p.verifyDB.Flush(string(blk.HeadHash()))
		if err != nil {
			return fmt.Errorf("flush stateDB failed, err:%v", err)
		}
		err = p.baseVariable.TxDB().Push(blk.Txs, blk.Receipts)
		if err != nil {
			return fmt.Errorf("push tx and txr into TxDB failed, err:%v", err)
		}
		p.baseVariable.SetMode(global.ModeNormal)
		return nil
	}
	return fmt.Errorf("not genesis block")
}
func (p *PoB) blockLoop() {
	ilog.Infof("start blockloop")
	for {
		select {
		case incomingMessage, ok := <-p.chRecvBlock:
			if !ok {
				ilog.Infof("chRecvBlock has closed")
				return
			}
			var blk block.Block
			err := blk.Decode(incomingMessage.Data())
			if err != nil {
				ilog.Error("fail to decode block")
				continue
			}
			ilog.Info(p.baseVariable.Mode())
			if p.baseVariable.Mode() == global.ModeFetchGenesis {
				err = p.handleGenesisBlock(&blk)
				if err != nil {
					ilog.Error(err)
					blkReq := &message.RequestBlock{
						BlockNumber: 0,
						BlockHash:   common.Base58Decode(p.baseVariable.Config().Genesis.GenesisHash),
					}
					bytes, err := blkReq.Encode()
					if err != nil {
						ilog.Errorf("fail to encode blkReq, %v", err)
						continue
					}
					p.p2pService.Broadcast(bytes, p2p.NewBlockRequest, p2p.UrgentMessage)
				}
				continue
			}
			if incomingMessage.Type() == p2p.NewBlock {
				ilog.Info("received new block, block number: ", blk.Head.Number)
				timer, ok := p.blockReqMap.Load(string(blk.HeadHash()))
				if ok {
					timer.(*time.Timer).Stop()
				} else {
					ilog.Info("block not in block request map, block number: ", blk.Head.Number)
					_, err := p.blockCache.Find(blk.HeadHash())
					if err == nil {
						ilog.Debug("duplicate block")
						continue
					}
					err = verifyBasics(blk.Head, blk.Sign)
					if err != nil {
						ilog.Debugf("fail to verify blocks, err:%v", err)
						continue
					}
					blkByte, err := blk.EncodeHead()
					if err != nil {
						ilog.Error("fail to encode block head")
						continue
					}
					p.p2pService.Broadcast(blkByte, p2p.NewBlockHead, p2p.UrgentMessage)
				}
				err = p.handleRecvBlock(&blk)
				p.blockReqMap.Delete(string(blk.HeadHash()))
				if err != nil && err != errSingle {
					ilog.Debugf("received new block error, err:%v", err)
					continue
				}
				if err == errSingle {
					if need, start, end := p.synchronizer.NeedSync(blk.Head.Number); need {
						go p.synchronizer.SyncBlocks(start, end)
					}
				}
			}
			if incomingMessage.Type() == p2p.SyncBlockResponse {
				ilog.Info("received sync block, block number: ", blk.Head.Number)
				_, err := p.blockCache.Find(blk.HeadHash())
				if err == nil {
					ilog.Debug(errors.New("duplicate block"))
					go p.synchronizer.OnBlockConfirmed(string(blk.HeadHash()), incomingMessage.From())
					continue
				}
				err = verifyBasics(blk.Head, blk.Sign)
				if err != nil {
					ilog.Debugf("fail to verify blocks, err:%v", err)
					continue
				}
				err = p.handleRecvBlock(&blk)
				if err != nil && err != errSingle {
					ilog.Debugf("received sync block error, err:%v", err)
					continue
				}
				go p.synchronizer.OnBlockConfirmed(string(blk.HeadHash()), incomingMessage.From())
			}
			go p.synchronizer.CheckSyncProcess()
		case blk, ok := <-p.chGenBlock:
			if !ok {
				ilog.Infof("chGenBlock has closed")
				return
			}
			ilog.Info("block from myself, block number: ", blk.Head.Number)
			err := p.handleRecvBlock(blk)
			if err != nil {
				ilog.Debugf("received new block error, err:%v", err)
				continue
			}
		case <-p.exitSignal:
			return
		}
	}
}

func (p *PoB) scheduleLoop() {
	nextSchedule := timeUntilNextSchedule(time.Now().UnixNano())
	ilog.Infof("nextSchedule: %.2f", time.Duration(nextSchedule).Seconds())
	for {
		select {
		case <-time.After(time.Duration(nextSchedule)):
			ilog.Infof("nextSchedule: %.2f", time.Duration(nextSchedule).Seconds())
			ilog.Info(p.baseVariable.Mode())
			metricsMode.Set(float64(p.baseVariable.Mode()), nil)
			if witnessOfSec(time.Now().Unix()) == p.account.ID {
				if p.baseVariable.Mode() == global.ModeNormal {
					blk, err := generateBlock(p.account, p.blockCache.Head().Block, p.txPool, p.produceDB)
					ilog.Infof("gen block:%v", blk.Head.Number)
					if err != nil {
						ilog.Error(err.Error())
						continue
					}
					p.chGenBlock <- blk
					blkByte, err := blk.Encode()
					if err != nil {
						ilog.Error(err.Error())
						continue
					}
					go p.p2pService.Broadcast(blkByte, p2p.NewBlock, p2p.UrgentMessage)
				}
				time.Sleep(common.SlotLength * time.Second)
			}
			nextSchedule = timeUntilNextSchedule(time.Now().UnixNano())
			ilog.Infof("nextSchedule: %.2f", time.Duration(nextSchedule).Seconds())
		case <-p.exitSignal:
			return
		}
	}
}

func (p *PoB) handleRecvBlock(blk *block.Block) error {
	parent, err := p.blockCache.Find(blk.Head.ParentHash)
	p.blockCache.Add(blk)
	staticProperty.addSlot(blk.Head.Time)
	if err == nil && parent.Type == blockcache.Linked {
		return p.addExistingBlock(blk, parent.Block)
	}
	return errSingle
}

func (p *PoB) addExistingBlock(blk *block.Block, parentBlock *block.Block) error {
	node, _ := p.blockCache.Find(blk.HeadHash())
	ok := p.verifyDB.Checkout(string(blk.HeadHash()))
	if !ok {
		p.verifyDB.Checkout(string(blk.Head.ParentHash))
		err := verifyBlock(blk, parentBlock, p.blockCache.LinkedRoot().Block, p.txPool, p.verifyDB)
		if err != nil {
			p.blockCache.Del(node)
			ilog.Error(err.Error())
			return err
		}
		p.verifyDB.Tag(string(blk.HeadHash()))
	}
	p.blockCache.Link(node)
	p.updateInfo(node)
	for child := range node.Children {
		p.addExistingBlock(child.Block, node.Block)
	}
	return nil
}

func (p *PoB) updateInfo(node *blockcache.BlockCacheNode) {
	updateWaterMark(node)
	updateLib(node, p.blockCache)
	p.txPool.AddLinkedNode(node, p.blockCache.Head())
}
