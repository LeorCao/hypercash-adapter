/*
 * Copyright 2018 The openwallet Authors
 * This file is part of the openwallet library.
 *
 * The openwallet library is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Lesser General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * The openwallet library is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
 * GNU Lesser General Public License for more details.
 */

package hypercash

import (
	"errors"
	"fmt"
	"github.com/tidwall/gjson"
	"path/filepath"
	"strings"
	"time"

	"github.com/asdine/storm"
	"github.com/blocktree/openwallet/common"
	"github.com/blocktree/openwallet/openwallet"
	"github.com/shopspring/decimal"
)

const (
	blockchainBucket = "blockchain" //区块链数据集合
	//periodOfTask      = 5 * time.Second //定时任务执行隔间
	maxExtractingSize = 1 //并发的扫描线程数

	RPCServerCore     = 0 //RPC服务，bitcoin核心钱包
	RPCServerExplorer = 1 //RPC服务，insight-API
)

//HCBlockScanner bitcoin的区块链扫描器
type HCBlockScanner struct {
	*openwallet.BlockScannerBase

	CurrentBlockHeight   uint64         //当前区块高度
	extractingCH         chan struct{}  //扫描工作令牌
	wm                   *WalletManager //钱包管理者
	IsScanMemPool        bool           //是否扫描交易池
	RescanLastBlockCount uint64         //重扫上N个区块数量
	//socketIO             *gosocketio.Client //socketIO客户端
	//setupSocketIOOnce    sync.Once
	//stopSocketIO         chan struct{}

	//用于实现浏览器
	IsSkipFailedBlock bool                                   //是否跳过失败区块
	HCBlockObservers  map[HCBlockScanNotificationObject]bool //观察者
}

//ExtractResult 扫描完成的提取结果
type ExtractResult struct {
	extractData     map[string]*openwallet.TxExtractData
	extractOmniData map[string]*openwallet.TxExtractData //代币交易
	TxID            string
	BlockHeight     uint64
	Success         bool
}

//SaveResult 保存结果
type SaveResult struct {
	TxID        string
	BlockHeight uint64
	Success     bool
}

//NewHCBlockScanner 创建区块链扫描器
func NewHCBlockScanner(wm *WalletManager) *HCBlockScanner {
	bs := HCBlockScanner{
		BlockScannerBase: openwallet.NewBlockScannerBase(),
	}

	bs.extractingCH = make(chan struct{}, maxExtractingSize)
	bs.wm = wm
	bs.IsScanMemPool = true
	bs.RescanLastBlockCount = 0
	//bs.stopSocketIO = make(chan struct{})
	bs.HCBlockObservers = make(map[HCBlockScanNotificationObject]bool)
	//bs.RPCServer = RPCServerCore

	//设置扫描任务
	bs.SetTask(bs.ScanBlockTask)

	return &bs
}

//SetRescanBlockHeight 重置区块链扫描高度
func (bs *HCBlockScanner) SetRescanBlockHeight(height uint64) error {
	height = height - 1
	if height < 0 {
		return errors.New("block height to rescan must greater than 0.")
	}

	hash, err := bs.wm.GetBlockHash(height)
	if err != nil {
		return err
	}

	bs.wm.SaveLocalNewBlock(height, hash)

	return nil
}

//ScanBlockTask 扫描任务
func (bs *HCBlockScanner) ScanBlockTask() {

	//获取本地区块高度
	blockHeader, err := bs.GetScannedBlockHeader()
	if err != nil {
		bs.wm.Log.Std.Info("block scanner can not get new block height; unexpected error: %v", err)
		return
	}

	currentHeight := blockHeader.Height
	currentHash := blockHeader.Hash

	for {

		if !bs.Scanning {
			//区块扫描器已暂停，马上结束本次任务
			return
		}

		//获取最大高度
		maxHeight, err := bs.wm.GetBlockHeight()
		if err != nil {
			//下一个高度找不到会报异常
			bs.wm.Log.Std.Info("block scanner can not get rpc-server block height; unexpected error: %v", err)
			break
		}

		//是否已到最新高度
		if currentHeight >= maxHeight {
			bs.wm.Log.Std.Info("block scanner has scanned full chain data. Current height: %d", maxHeight)
			break
		}

		//继续扫描下一个区块
		currentHeight = currentHeight + 1

		bs.wm.Log.Std.Info("block scanner scanning height: %d ...", currentHeight)

		hash, err := bs.wm.GetBlockHash(currentHeight)
		if err != nil {
			//下一个高度找不到会报异常
			bs.wm.Log.Std.Info("block scanner can not get new block hash; unexpected error: %v", err)
			break
		}

		if bs.wm.Config.OmniSupport {
			//判断omni的区块高度是否一致
			omniBlockHash, err := bs.wm.GetOmniBlockHash(currentHeight)
			if err != nil {
				bs.wm.Log.Std.Error("omni block is not synced to the same height of mainnet")
				return
			}

			//判断omni的hash是否与hc节点的hash一致
			if omniBlockHash != hash {
				bs.wm.Log.Std.Error("omni block is not synced to the same hash of mainnet")
				return
			}
		}

		block, err := bs.wm.GetBlock(hash)
		if err != nil {
			bs.wm.Log.Std.Info("block scanner can not get new block data; unexpected error: %v", err)

			//记录未扫区块
			unscanRecord := NewUnscanRecord(currentHeight, "", err.Error())
			bs.SaveUnscanRecord(unscanRecord)
			bs.wm.Log.Std.Info("block height: %d extract failed.", currentHeight)
			continue
		}

		isFork := false

		//判断hash是否上一区块的hash
		if currentHash != block.Previousblockhash {

			bs.wm.Log.Std.Info("block has been fork on height: %d.", currentHeight)
			bs.wm.Log.Std.Info("block height: %d local hash = %s ", currentHeight-1, currentHash)
			bs.wm.Log.Std.Info("block height: %d mainnet hash = %s ", currentHeight-1, block.Previousblockhash)

			bs.wm.Log.Std.Info("delete recharge records on block height: %d.", currentHeight-1)

			//查询本地分叉的区块
			forkBlock, _ := bs.wm.GetLocalBlock(currentHeight - 1)

			//删除上一区块链的所有充值记录
			//bs.DeleteRechargesByHeight(currentHeight - 1)
			//删除上一区块链的未扫记录
			bs.wm.DeleteUnscanRecord(currentHeight - 1)
			currentHeight = currentHeight - 2 //倒退2个区块重新扫描
			if currentHeight <= 0 {
				currentHeight = 1
			}

			localBlock, err := bs.wm.GetLocalBlock(currentHeight)
			if err != nil {
				bs.wm.Log.Std.Error("block scanner can not get local block; unexpected error: %v", err)

				//查找core钱包的RPC
				bs.wm.Log.Info("block scanner prev block height:", currentHeight)

				prevHash, err := bs.wm.GetBlockHash(currentHeight)
				if err != nil {
					bs.wm.Log.Std.Error("block scanner can not get prev block; unexpected error: %v", err)
					break
				}

				localBlock, err = bs.wm.GetBlock(prevHash)
				if err != nil {
					bs.wm.Log.Std.Error("block scanner can not get prev block; unexpected error: %v", err)
					break
				}

			}

			//重置当前区块的hash
			currentHash = localBlock.Hash

			bs.wm.Log.Std.Info("rescan block on height: %d, hash: %s .", currentHeight, currentHash)

			//重新记录一个新扫描起点
			bs.wm.SaveLocalNewBlock(localBlock.Height, localBlock.Hash)

			isFork = true

			if forkBlock != nil {

				//通知分叉区块给观测者，异步处理
				bs.newBlockNotify(forkBlock, isFork)
			}

		} else {

			err = bs.BatchExtractTransaction(block.Height, block.Hash, block.tx)
			if err != nil {
				bs.wm.Log.Std.Info("block scanner can not extractRechargeRecords; unexpected error: %v", err)
			}

			//重置当前区块的hash
			currentHash = hash

			//保存本地新高度
			bs.wm.SaveLocalNewBlock(currentHeight, currentHash)
			bs.wm.SaveLocalBlock(block)

			isFork = false

			//通知新区块给观测者，异步处理
			bs.newBlockNotify(block, isFork)
		}

	}

	//重扫前N个块，为保证记录找到
	for i := currentHeight - bs.RescanLastBlockCount; i < currentHeight; i++ {
		bs.scanBlock(i)
	}

	if bs.IsScanMemPool {
		//扫描交易内存池
		bs.ScanTxMemPool()
	}

	//重扫失败区块
	bs.RescanFailedRecord()

}

//ScanBlock 扫描指定高度区块
func (bs *HCBlockScanner) ScanBlock(height uint64) error {

	block, err := bs.scanBlock(height)
	if err != nil {
		return err
	}

	//通知新区块给观测者，异步处理
	bs.newBlockNotify(block, false)

	return nil
}

func (bs *HCBlockScanner) scanBlock(height uint64) (*Block, error) {

	hash, err := bs.wm.GetBlockHash(height)
	if err != nil {
		//下一个高度找不到会报异常
		bs.wm.Log.Std.Info("block scanner can not get new block hash; unexpected error: %v", err)
		return nil, err
	}

	block, err := bs.wm.GetBlock(hash)
	if err != nil {
		bs.wm.Log.Std.Info("block scanner can not get new block data; unexpected error: %v", err)

		//记录未扫区块
		unscanRecord := NewUnscanRecord(height, "", err.Error())
		bs.SaveUnscanRecord(unscanRecord)
		bs.wm.Log.Std.Info("block height: %d extract failed.", height)
		return nil, err
	}

	bs.wm.Log.Std.Info("block scanner scanning height: %d ...", block.Height)

	err = bs.BatchExtractTransaction(block.Height, block.Hash, block.tx)
	if err != nil {
		bs.wm.Log.Std.Info("block scanner can not extractRechargeRecords; unexpected error: %v", err)
	}

	//保存区块
	//bs.wm.SaveLocalBlock(block)

	return block, nil
}

//ScanTxMemPool 扫描交易内存池
func (bs *HCBlockScanner) ScanTxMemPool() {

	bs.wm.Log.Std.Info("block scanner scanning mempool ...")

	//提取未确认的交易单
	txIDsInMemPool, err := bs.wm.GetTxIDsInMemPool()
	if err != nil {
		bs.wm.Log.Std.Info("block scanner can not get mempool data; unexpected error: %v", err)
		return
	}

	if txIDsInMemPool == nil || len(txIDsInMemPool) == 0 {
		return
	}

	err = bs.BatchExtractTransaction(0, "", txIDsInMemPool)
	if err != nil {
		bs.wm.Log.Std.Info("block scanner can not extractRechargeRecords; unexpected error: %v", err)
	}

}

//rescanFailedRecord 重扫失败记录
func (bs *HCBlockScanner) RescanFailedRecord() {

	var (
		blockMap = make(map[uint64][]string)
	)

	list, err := bs.wm.GetUnscanRecords()
	if err != nil {
		bs.wm.Log.Std.Info("block scanner can not get rescan data; unexpected error: %v", err)
	}

	//组合成批处理
	for _, r := range list {

		if _, exist := blockMap[r.BlockHeight]; !exist {
			blockMap[r.BlockHeight] = make([]string, 0)
		}

		if len(r.TxID) > 0 {
			arr := blockMap[r.BlockHeight]
			arr = append(arr, r.TxID)

			blockMap[r.BlockHeight] = arr
		}
	}

	for height, txs := range blockMap {

		if height == 0 {
			continue
		}

		var hash string

		bs.wm.Log.Std.Info("block scanner rescanning height: %d ...", height)

		if len(txs) == 0 {

			hash, err := bs.wm.GetBlockHash(height)
			if err != nil {
				//下一个高度找不到会报异常
				bs.wm.Log.Std.Info("block scanner can not get new block hash; unexpected error: %v", err)
				continue
			}

			block, err := bs.wm.GetBlock(hash)
			if err != nil {
				bs.wm.Log.Std.Info("block scanner can not get new block data; unexpected error: %v", err)
				continue
			}

			txs = block.tx
		}

		err = bs.BatchExtractTransaction(height, hash, txs)
		if err != nil {
			bs.wm.Log.Std.Info("block scanner can not extractRechargeRecords; unexpected error: %v", err)
			continue
		}

		//删除未扫记录
		bs.wm.DeleteUnscanRecord(height)
	}

	//删除未没有找到交易记录的重扫记录
	bs.wm.DeleteUnscanRecordNotFindTX()
}

//newBlockNotify 获得新区块后，通知给观测者
func (bs *HCBlockScanner) newBlockNotify(block *Block, isFork bool) {
	header := block.BlockHeader(bs.wm.Symbol())
	header.Fork = isFork
	bs.NewBlockNotify(header)
}

//BatchExtractTransaction 批量提取交易单
//hypercash 1M的区块链可以容纳3000笔交易，批量多线程处理，速度更快
func (bs *HCBlockScanner) BatchExtractTransaction(blockHeight uint64, blockHash string, txs []string) error {

	var (
		quit       = make(chan struct{})
		done       = 0 //完成标记
		failed     = 0
		shouldDone = len(txs) //需要完成的总数
	)

	if len(txs) == 0 {
		return errors.New("BatchExtractTransaction block is nil.")
	}

	//生产通道
	producer := make(chan ExtractResult)
	defer close(producer)

	//消费通道
	worker := make(chan ExtractResult)
	defer close(worker)

	//保存工作
	saveWork := func(height uint64, result chan ExtractResult) {
		//回收创建的地址
		for gets := range result {

			if gets.Success {

				notifyErr := bs.newExtractDataNotify(height, gets.extractData)
				//saveErr := bs.SaveRechargeToWalletDB(height, gets.Recharges)
				if notifyErr != nil {
					failed++ //标记保存失败数
					bs.wm.Log.Std.Info("newExtractDataNotify unexpected error: %v", notifyErr)
				}

				notifyErr = nil
				notifyErr = bs.newExtractDataNotify(height, gets.extractOmniData)
				if notifyErr != nil {
					failed++ //标记保存失败数
					bs.wm.Log.Std.Info("newExtractDataNotify unexpected error: %v", notifyErr)
				}

			} else {
				//记录未扫区块
				unscanRecord := NewUnscanRecord(height, "", "")
				bs.SaveUnscanRecord(unscanRecord)
				bs.wm.Log.Std.Info("block height: %d extract failed.", height)
				failed++ //标记保存失败数
			}
			//累计完成的线程数
			done++
			if done == shouldDone {
				//bs.wm.Log.Std.Info("done = %d, shouldDone = %d ", done, len(txs))
				close(quit) //关闭通道，等于给通道传入nil
			}
		}
	}

	//提取工作
	extractWork := func(eblockHeight uint64, eBlockHash string, mTxs []string, eProducer chan ExtractResult) {
		for _, txid := range mTxs {
			bs.extractingCH <- struct{}{}
			//shouldDone++
			go func(mBlockHeight uint64, mTxid string, end chan struct{}, mProducer chan<- ExtractResult) {

				//导出提出的交易
				mProducer <- bs.ExtractTransaction(mBlockHeight, eBlockHash, mTxid, bs.ScanAddressFunc)
				//释放
				<-end

			}(eblockHeight, txid, bs.extractingCH, eProducer)
		}
	}

	/*	开启导出的线程	*/

	//独立线程运行消费
	go saveWork(blockHeight, worker)

	//独立线程运行生产
	go extractWork(blockHeight, blockHash, txs, producer)

	//以下使用生产消费模式
	bs.extractRuntime(producer, worker, quit)

	if failed > 0 {
		return fmt.Errorf("block scanner saveWork failed")
	} else {
		return nil
	}

	//return nil
}

//extractRuntime 提取运行时
func (bs *HCBlockScanner) extractRuntime(producer chan ExtractResult, worker chan ExtractResult, quit chan struct{}) {

	var (
		values = make([]ExtractResult, 0)
	)

	for {

		var activeWorker chan<- ExtractResult
		var activeValue ExtractResult

		//当数据队列有数据时，释放顶部，传输给消费者
		if len(values) > 0 {
			activeWorker = worker
			activeValue = values[0]

		}

		select {

		//生成者不断生成数据，插入到数据队列尾部
		case pa := <-producer:
			values = append(values, pa)
		case <-quit:
			//退出
			//bs.wm.Log.Std.Info("block scanner have been scanned!")
			return
		case activeWorker <- activeValue:
			//wm.Log.Std.Info("Get %d", len(activeValue))
			values = values[1:]
		}
	}

}

//ExtractTransaction 提取交易单
func (bs *HCBlockScanner) ExtractTransaction(blockHeight uint64, blockHash string, txid string, scanAddressFunc openwallet.BlockScanAddressFunc) ExtractResult {

	var (
		result = ExtractResult{
			BlockHeight:     blockHeight,
			TxID:            txid,
			extractData:     make(map[string]*openwallet.TxExtractData),
			extractOmniData: make(map[string]*openwallet.TxExtractData),
		}
	)

	//bs.wm.Log.Std.Debug("block scanner scanning tx: %s ...", txid)
	//获取bitcoin的交易单
	trx, err := bs.wm.GetTransaction(txid)

	if err != nil {
		bs.wm.Log.Std.Info("block scanner can not extract transaction data; unexpected error: %v", err)
		result.Success = false
		return result
	}

	//优先使用传入的高度
	if blockHeight > 0 && trx.BlockHeight == 0 {
		trx.BlockHeight = blockHeight
		trx.BlockHash = blockHash
	}

	//bs.wm.Log.Debug("start extractTransaction")
	bs.extractTransaction(trx, &result, scanAddressFunc)
	//bs.wm.Log.Debug("end extractTransaction")

	if bs.wm.Config.OmniSupport {
		//获取omni的交易单
		omniTrx, err := bs.wm.GetOmniTransaction(txid)
		if err != nil {
			//bs.wm.Log.Std.Info("block scanner can not extract omni transaction data; unexpected error: %v", err)
			result.Success = true
			return result
		}

		bs.extractOmniTransaction(omniTrx, &result, scanAddressFunc)
	}

	return result

}

//extractOmniTransaction 提取Omni交易单
func (bs *HCBlockScanner) extractOmniTransaction(trx *OmniTransaction, result *ExtractResult, scanAddressFunc openwallet.BlockScanAddressFunc) {

	var (
		success = true
	)

	if trx == nil {
		success = true
	} else {

		if trx.Valid {
			createAt := time.Now().Unix()
			propertyID := common.NewString(trx.PropertyId).String()
			contractId := openwallet.GenContractID(bs.wm.Symbol(), propertyID)

			coin := openwallet.Coin{
				Symbol:     bs.wm.Symbol(),
				IsContract: true,
				ContractID: contractId,
				Contract: openwallet.SmartContract{
					ContractID: contractId,
					Address:    propertyID,
					Protocol:   "omnicore",
					Symbol:     bs.wm.Symbol(),
				},
			}

			amountDec, _ := decimal.NewFromString(trx.Amount)
			amountDec = amountDec.Shift(bs.wm.Decimal())
			amount := amountDec.StringFixed(0)
			sourceKey, ok := scanAddressFunc(trx.SendingAddress)
			if ok {
				input := openwallet.TxInput{}
				input.TxID = trx.TxID
				input.Address = trx.SendingAddress
				//transaction.AccountID = a.AccountID
				input.Amount = amount
				input.Coin = coin
				input.Index = 0
				input.Sid = openwallet.GenTxInputSID(trx.TxID, bs.wm.Symbol(), contractId, 0)
				//input.Sid = base64.StdEncoding.EncodeToString(crypto.SHA1([]byte(fmt.Sprintf("input_%s_%d_%s", result.TxID, i, addr))))
				input.CreateAt = createAt
				//在哪个区块高度时消费
				input.BlockHeight = trx.Block
				input.BlockHash = trx.BlockHash

				//transactions = append(transactions, &transaction)

				ed := result.extractOmniData[sourceKey]
				if ed == nil {
					ed = openwallet.NewBlockExtractData()
					result.extractOmniData[sourceKey] = ed
				}

				ed.TxInputs = append(ed.TxInputs, &input)

			}

			sourceKey2, ok2 := scanAddressFunc(trx.ReferenceAddress)
			if ok2 {
				output := openwallet.TxOutPut{}
				output.TxID = trx.TxID
				output.Address = trx.ReferenceAddress
				//transaction.AccountID = a.AccountID
				output.Amount = amount

				output.Coin = coin
				output.Index = 0
				output.Sid = openwallet.GenTxOutPutSID(trx.TxID, bs.wm.Symbol(), contractId, 0)
				//input.Sid = base64.StdEncoding.EncodeToString(crypto.SHA1([]byte(fmt.Sprintf("input_%s_%d_%s", result.TxID, i, addr))))
				output.CreateAt = createAt
				//在哪个区块高度时消费
				output.BlockHeight = trx.Block
				output.BlockHash = trx.BlockHash

				//transactions = append(transactions, &transaction)

				ed := result.extractOmniData[sourceKey2]
				if ed == nil {
					ed = openwallet.NewBlockExtractData()
					result.extractOmniData[sourceKey2] = ed
				}

				ed.TxOutputs = append(ed.TxOutputs, &output)
			}

			blocktime := trx.BlockTime

			for _, extractData := range result.extractOmniData {
				tx := &openwallet.Transaction{
					From:        []string{trx.SendingAddress + ":" + amount},
					To:          []string{trx.ReferenceAddress + ":" + amount},
					Fees:        "0",
					Coin:        coin,
					BlockHash:   trx.BlockHash,
					BlockHeight: trx.Block,
					TxID:        trx.TxID,
					Decimal:     0,
					ConfirmTime: blocktime,
					Status:      openwallet.TxStatusSuccess,
				}
				wxID := openwallet.GenTransactionWxID(tx)
				tx.WxID = wxID
				extractData.Transaction = tx

				//bs.wm.Log.Debug("Transaction:", extractData.Transaction)
			}
		}

		success = true

	}

	result.Success = success

}

//ExtractTransactionData 提取交易单
func (bs *HCBlockScanner) extractTransaction(trx *Transaction, result *ExtractResult, scanAddressFunc openwallet.BlockScanAddressFunc) {

	var (
		success = true
	)

	if trx == nil {
		//记录哪个区块哪个交易单没有完成扫描
		success = false
	} else {

		vin := trx.Vins
		blocktime := trx.Blocktime

		//检查交易单输入信息是否完整，不完整查上一笔交易单的输出填充数据
		for _, input := range vin {

			if len(input.Coinbase) > 0 {
				//coinbase skip
				success = true
				break
			}

			//如果input中没有地址，需要查上一笔交易的output提取
			if len(input.Addr) == 0 {

				intxid := input.TxID
				vout := input.Vout

				preTx, err := bs.wm.GetTransaction(intxid)
				if err != nil {
					success = false
					break
				} else {
					preVouts := preTx.Vouts
					if len(preVouts) > int(vout) {
						preOut := preVouts[vout]
						input.Addr = preOut.Addr
						input.Value = preOut.Value
						//vinout = append(vinout, output[vout])
						success = true

						//bs.wm.Log.Debug("GetTxOut:", output[vout])

					}
				}

			}

		}

		if success {

			//提取出账部分记录
			from, totalSpent := bs.extractTxInput(trx, result, scanAddressFunc)
			//bs.wm.Log.Debug("from:", from, "totalSpent:", totalSpent)

			//提取入账部分记录
			to, totalReceived := bs.extractTxOutput(trx, result, scanAddressFunc)
			//bs.wm.Log.Debug("to:", to, "totalReceived:", totalReceived)

			for _, extractData := range result.extractData {
				tx := &openwallet.Transaction{
					From: from,
					To:   to,
					Fees: totalSpent.Sub(totalReceived).StringFixed(bs.wm.Decimal()),
					Coin: openwallet.Coin{
						Symbol:     bs.wm.Symbol(),
						IsContract: false,
					},
					BlockHash:   trx.BlockHash,
					BlockHeight: trx.BlockHeight,
					TxID:        trx.TxID,
					Decimal:     bs.wm.Decimal(),
					ConfirmTime: blocktime,
					Status:      openwallet.TxStatusSuccess,
				}
				wxID := openwallet.GenTransactionWxID(tx)
				tx.WxID = wxID
				extractData.Transaction = tx

				//bs.wm.Log.Debug("Transaction:", extractData.Transaction)
			}

		}

		success = true

	}
	result.Success = success
}

//ExtractTxInput 提取交易单输入部分
func (bs *HCBlockScanner) extractTxInput(trx *Transaction, result *ExtractResult, scanAddressFunc openwallet.BlockScanAddressFunc) ([]string, decimal.Decimal) {

	//vin := trx.Get("vin")

	var (
		from        = make([]string, 0)
		totalAmount = decimal.Zero
	)

	createAt := time.Now().Unix()
	for i, output := range trx.Vins {

		//in := vin[i]

		txid := output.TxID
		vout := output.Vout
		//
		//output, err := bs.wm.GetTxOut(txid, vout)
		//if err != nil {
		//	return err
		//}

		amount := output.Value
		addr := output.Addr
		sourceKey, ok := scanAddressFunc(addr)
		if ok {
			input := openwallet.TxInput{}
			input.SourceTxID = txid
			input.SourceIndex = vout
			input.TxID = result.TxID
			input.Address = addr
			//transaction.AccountID = a.AccountID
			input.Amount = amount
			input.Coin = openwallet.Coin{
				Symbol:     bs.wm.Symbol(),
				IsContract: false,
			}
			input.Index = output.N
			input.Sid = openwallet.GenTxInputSID(txid, bs.wm.Symbol(), "", uint64(i))
			//input.Sid = base64.StdEncoding.EncodeToString(crypto.SHA1([]byte(fmt.Sprintf("input_%s_%d_%s", result.TxID, i, addr))))
			input.CreateAt = createAt
			//在哪个区块高度时消费
			input.BlockHeight = trx.BlockHeight
			input.BlockHash = trx.BlockHash

			//transactions = append(transactions, &transaction)

			ed := result.extractData[sourceKey]
			if ed == nil {
				ed = openwallet.NewBlockExtractData()
				result.extractData[sourceKey] = ed
			}

			ed.TxInputs = append(ed.TxInputs, &input)

		}

		from = append(from, addr+":"+amount)
		dAmount, _ := decimal.NewFromString(amount)
		totalAmount = totalAmount.Add(dAmount)

	}
	return from, totalAmount
}

//ExtractTxInput 提取交易单输入部分
func (bs *HCBlockScanner) extractTxOutput(trx *Transaction, result *ExtractResult, scanAddressFunc openwallet.BlockScanAddressFunc) ([]string, decimal.Decimal) {

	var (
		to          = make([]string, 0)
		totalAmount = decimal.Zero
	)

	confirmations := trx.Confirmations
	vout := trx.Vouts
	txid := trx.TxID
	//bs.wm.Log.Debug("vout:", vout.Array())
	createAt := time.Now().Unix()
	for _, output := range vout {

		amount := output.Value
		n := output.N
		addr := output.Addr
		sourceKey, ok := scanAddressFunc(addr)
		if ok {

			//a := wallet.GetAddress(addr)
			//if a == nil {
			//	continue
			//}

			outPut := openwallet.TxOutPut{}
			outPut.TxID = txid
			outPut.Address = addr
			//transaction.AccountID = a.AccountID
			outPut.Amount = amount
			outPut.Coin = openwallet.Coin{
				Symbol:     bs.wm.Symbol(),
				IsContract: false,
			}
			outPut.Index = n
			outPut.Sid = openwallet.GenTxOutPutSID(txid, bs.wm.Symbol(), "", n)
			//outPut.Sid = base64.StdEncoding.EncodeToString(crypto.SHA1([]byte(fmt.Sprintf("output_%s_%d_%s", txid, n, addr))))

			//保存utxo到扩展字段
			outPut.SetExtParam("scriptPubKey", output.ScriptPubKey)
			outPut.CreateAt = createAt
			outPut.BlockHeight = trx.BlockHeight
			outPut.BlockHash = trx.BlockHash
			outPut.Confirm = int64(confirmations)

			//transactions = append(transactions, &transaction)

			ed := result.extractData[sourceKey]
			if ed == nil {
				ed = openwallet.NewBlockExtractData()
				result.extractData[sourceKey] = ed
			}

			ed.TxOutputs = append(ed.TxOutputs, &outPut)

		}

		to = append(to, addr+":"+amount)
		dAmount, _ := decimal.NewFromString(amount)
		totalAmount = totalAmount.Add(dAmount)

	}

	return to, totalAmount
}

//newExtractDataNotify 发送通知
func (bs *HCBlockScanner) newExtractDataNotify(height uint64, extractData map[string]*openwallet.TxExtractData) error {

	for o, _ := range bs.Observers {
		for key, data := range extractData {
			err := o.BlockExtractDataNotify(key, data)
			if err != nil {
				bs.wm.Log.Error("BlockExtractDataNotify unexpected error:", err)
				//记录未扫区块
				unscanRecord := NewUnscanRecord(height, "", "ExtractData Notify failed.")
				err = bs.SaveUnscanRecord(unscanRecord)
				if err != nil {
					bs.wm.Log.Std.Error("block height: %d, save unscan record failed. unexpected error: %v", height, err.Error())
				}

			}
		}
	}

	return nil
}

//DeleteUnscanRecordNotFindTX 删除未没有找到交易记录的重扫记录
func (wm *WalletManager) DeleteUnscanRecordNotFindTX() error {

	//删除找不到交易单
	reason := "[-5]No information available about transaction"

	//获取本地区块高度
	db, err := storm.Open(filepath.Join(wm.Config.dbPath, wm.Config.BlockchainFile))
	if err != nil {
		return err
	}
	defer db.Close()

	var list []*UnscanRecord
	err = db.All(&list)
	if err != nil {
		return err
	}

	tx, err := db.Begin(true)
	if err != nil {
		return err
	}
	for _, r := range list {
		if strings.HasPrefix(r.Reason, reason) {
			tx.DeleteStruct(r)
		}
	}
	return tx.Commit()
}

//SaveRechargeToWalletDB 保存交易单内的充值记录到钱包数据库
//func (bs *HCBlockScanner) SaveRechargeToWalletDB(height uint64, list []*openwallet.Recharge) error {
//
//	for _, r := range list {
//
//		//accountID := "W4ruoAyS5HdBMrEeeHQTBxo4XtaAixheXQ"
//		wallet, ok := bs.GetWalletByAddress(r.Address)
//		if ok {
//
//			//a := wallet.GetAddress(r.Address)
//			//if a == nil {
//			//	continue
//			//}
//			//
//			//r.AccountID = a.AccountID
//
//			err := wallet.SaveUnreceivedRecharge(r)
//			//如果blockHash没有值，添加到重扫，避免遗留
//			if err != nil || len(r.BlockHash) == 0 {
//
//				//记录未扫区块
//				unscanRecord := NewUnscanRecord(height, r.TxID, "save to wallet failed.")
//				err = bs.SaveUnscanRecord(unscanRecord)
//				if err != nil {
//					bs.wm.Log.Std.Error("block height: %d, txID: %s save unscan record failed. unexpected error: %v", height, r.TxID, err.Error())
//				}
//
//			} else {
//				bs.wm.Log.Info("block scanner save blockHeight:", height, "txid:", r.TxID, "address:", r.Address, "successfully.")
//			}
//		} else {
//			return errors.New("address in wallet is not found")
//		}
//
//	}
//
//	return nil
//}

//GetScannedBlockHeader 获取当前扫描的区块头
func (bs *HCBlockScanner) GetScannedBlockHeader() (*openwallet.BlockHeader, error) {

	var (
		blockHeight uint64 = 0
		hash        string
		err         error
	)

	blockHeight, hash = bs.wm.GetLocalNewBlock()

	//如果本地没有记录，查询接口的高度
	if blockHeight == 0 {
		blockHeight, err = bs.wm.GetBlockHeight()
		if err != nil {

			return nil, err
		}

		//就上一个区块链为当前区块
		blockHeight = blockHeight - 1

		hash, err = bs.wm.GetBlockHash(blockHeight)
		if err != nil {
			return nil, err
		}
	}

	return &openwallet.BlockHeader{Height: blockHeight, Hash: hash}, nil
}

//GetCurrentBlockHeader 获取当前区块高度
func (bs *HCBlockScanner) GetCurrentBlockHeader() (*openwallet.BlockHeader, error) {

	var (
		blockHeight uint64 = 0
		hash        string
		err         error
	)

	blockHeight, err = bs.wm.GetBlockHeight()
	if err != nil {

		return nil, err
	}

	hash, err = bs.wm.GetBlockHash(blockHeight)
	if err != nil {
		return nil, err
	}

	return &openwallet.BlockHeader{Height: blockHeight, Hash: hash}, nil
}

func (bs *HCBlockScanner) GetGlobalMaxBlockHeight() uint64 {
	maxHeight, err := bs.wm.GetBlockHeight()
	if err != nil {
		bs.wm.Log.Std.Info("get global max block height error;unexpected error:%v", err)
		return 0
	}
	return maxHeight
}

//GetScannedBlockHeight 获取已扫区块高度
func (bs *HCBlockScanner) GetScannedBlockHeight() uint64 {
	localHeight, _ := bs.wm.GetLocalNewBlock()
	return localHeight
}

func (bs *HCBlockScanner) ExtractTransactionData(txid string, scanTargetFunc openwallet.BlockScanTargetFunc) (map[string][]*openwallet.TxExtractData, error) {

	scanAddressFunc := func(address string) (string, bool) {
		target := openwallet.ScanTarget{
			Address:          address,
			BalanceModelType: openwallet.BalanceModelTypeAddress,
		}
		return scanTargetFunc(target)
	}
	result := bs.ExtractTransaction(0, "", txid, scanAddressFunc)
	if !result.Success {
		return nil, fmt.Errorf("extract transaction failed")
	}
	extData := make(map[string][]*openwallet.TxExtractData)
	for key, data := range result.extractData {
		txs := extData[key]
		if txs == nil {
			txs = make([]*openwallet.TxExtractData, 0)
		}
		txs = append(txs, data)
		extData[key] = txs
	}
	return extData, nil
}

//DropRechargeRecords 清楚钱包的全部充值记录
//func (bs *HCBlockScanner) DropRechargeRecords(accountID string) error {
//	bs.mu.RLock()
//	defer bs.mu.RUnlock()
//
//	wallet, ok := bs.walletInScanning[accountID]
//	if !ok {
//		errMsg := fmt.Sprintf("accountID: %s wallet is not found", accountID)
//		return errors.New(errMsg)
//	}
//
//	return wallet.DropRecharge()
//}

//DeleteRechargesByHeight 删除某区块高度的充值记录
//func (bs *HCBlockScanner) DeleteRechargesByHeight(height uint64) error {
//
//	bs.mu.RLock()
//	defer bs.mu.RUnlock()
//
//	for _, wallet := range bs.walletInScanning {
//
//		list, err := wallet.GetRecharges(false, height)
//		if err != nil {
//			return err
//		}
//
//		db, err := wallet.OpenDB()
//		if err != nil {
//			return err
//		}
//
//		tx, err := db.Begin(true)
//		if err != nil {
//			return err
//		}
//
//		for _, r := range list {
//			err = db.DeleteStruct(&r)
//			if err != nil {
//				return err
//			}
//		}
//
//		tx.Commit()
//
//		db.Close()
//	}
//
//	return nil
//}

//SaveTxToWalletDB 保存交易记录到钱包数据库
func (bs *HCBlockScanner) SaveUnscanRecord(record *UnscanRecord) error {

	if record == nil {
		return errors.New("the unscan record to save is nil")
	}

	if record.BlockHeight == 0 {
		bs.wm.Log.Warn("unconfirmed transaction do not rescan")
		return nil
	}

	//获取本地区块高度
	db, err := storm.Open(filepath.Join(bs.wm.Config.dbPath, bs.wm.Config.BlockchainFile))
	if err != nil {
		return err
	}
	defer db.Close()

	return db.Save(record)
}

//GetWalletByAddress 获取地址对应的钱包
//func (bs *HCBlockScanner) GetWalletByAddress(address string) (*openwallet.Wallet, bool) {
//	bs.mu.RLock()
//	defer bs.mu.RUnlock()
//
//	account, ok := bs.addressInScanning[address]
//	if ok {
//		wallet, ok := bs.walletInScanning[account]
//		return wallet, ok
//
//	} else {
//		return nil, false
//	}
//}

//GetBlockHeight 获取区块链高度
func (wm *WalletManager) GetBlockHeight() (uint64, error) {

	if wm.Config.RPCServerType == RPCServerExplorer {
		return wm.getBlockHeightByExplorer()
	} else {
		return wm.getBlockHeightByCore()
	}
}

//getBlockHeightByCore 获取区块链高度
func (wm *WalletManager) getBlockHeightByCore() (uint64, error) {

	result, err := wm.WalletClient.Call("getblockcount", nil)
	if err != nil {
		return 0, err
	}

	return result.Uint(), nil
}

//GetLocalNewBlock 获取本地记录的区块高度和hash
func (wm *WalletManager) GetLocalNewBlock() (uint64, string) {

	var (
		blockHeight uint64 = 0
		blockHash   string = ""
	)

	//获取本地区块高度
	db, err := storm.Open(filepath.Join(wm.Config.dbPath, wm.Config.BlockchainFile))
	if err != nil {
		return 0, ""
	}
	defer db.Close()

	db.Get(blockchainBucket, "blockHeight", &blockHeight)
	db.Get(blockchainBucket, "blockHash", &blockHash)

	return blockHeight, blockHash
}

//SaveLocalNewBlock 记录区块高度和hash到本地
func (wm *WalletManager) SaveLocalNewBlock(blockHeight uint64, blockHash string) {

	//获取本地区块高度
	db, err := storm.Open(filepath.Join(wm.Config.dbPath, wm.Config.BlockchainFile))
	if err != nil {
		return
	}
	defer db.Close()

	db.Set(blockchainBucket, "blockHeight", &blockHeight)
	db.Set(blockchainBucket, "blockHash", &blockHash)
}

//SaveLocalBlock 记录本地新区块
func (wm *WalletManager) SaveLocalBlock(block *Block) {

	db, err := storm.Open(filepath.Join(wm.Config.dbPath, wm.Config.BlockchainFile))
	if err != nil {
		return
	}
	defer db.Close()

	db.Save(block)
}

//GetBlockHash 根据区块高度获得区块hash
func (wm *WalletManager) GetBlockHash(height uint64) (string, error) {

	if wm.Config.RPCServerType == RPCServerExplorer {
		return wm.getBlockHashByExplorer(height)
	} else {
		return wm.getBlockHashByCore(height)
	}
}

//getBlockHashByCore 根据区块高度获得区块hash
func (wm *WalletManager) getBlockHashByCore(height uint64) (string, error) {

	request := []interface{}{
		height,
	}

	result, err := wm.WalletClient.Call("getblockhash", request)
	if err != nil {
		return "", err
	}

	return result.String(), nil
}

//GetLocalBlock 获取本地区块数据
func (wm *WalletManager) GetLocalBlock(height uint64) (*Block, error) {

	var (
		block Block
	)

	db, err := storm.Open(filepath.Join(wm.Config.dbPath, wm.Config.BlockchainFile))
	if err != nil {
		return nil, err
	}
	defer db.Close()

	err = db.One("Height", height, &block)
	if err != nil {
		return nil, err
	}

	return &block, nil
}

//GetBlock 获取区块数据
func (wm *WalletManager) GetBlock(hash string) (*Block, error) {

	if wm.Config.RPCServerType == RPCServerExplorer {
		return wm.getBlockByExplorer(hash)
	} else {
		return wm.getBlockByCore(hash)
	}
}

//getBlockByCore 获取区块数据
func (wm *WalletManager) getBlockByCore(hash string, format ...uint64) (*Block, error) {

	request := []interface{}{
		hash,
	}

	if len(format) > 0 {
		request = append(request, format[0])
	}

	result, err := wm.WalletClient.Call("getblock", request)
	if err != nil {
		return nil, err
	}

	return wm.NewBlock(result), nil
}

//GetTxIDsInMemPool 获取待处理的交易池中的交易单IDs
func (wm *WalletManager) GetTxIDsInMemPool() ([]string, error) {

	if wm.Config.RPCServerType == RPCServerExplorer {
		return wm.getTxIDsInMemPoolByExplorer()
	} else {
		return wm.getTxIDsInMemPoolByCore()
	}
}

//getTxIDsInMemPoolByCore 获取待处理的交易池中的交易单IDs
func (wm *WalletManager) getTxIDsInMemPoolByCore() ([]string, error) {

	var (
		txids = make([]string, 0)
	)

	result, err := wm.WalletClient.Call("getrawmempool", nil)
	if err != nil {
		return nil, err
	}

	if !result.IsArray() {
		return nil, errors.New("no query record")
	}

	for _, txid := range result.Array() {
		txids = append(txids, txid.String())
	}

	return txids, nil
}

//GetTransaction 获取交易单
func (wm *WalletManager) GetTransaction(txid string) (*Transaction, error) {

	if wm.Config.RPCServerType == RPCServerExplorer {
		return wm.getTransactionByExplorer(txid)
	} else {
		return wm.getTransactionByCore(txid)
	}
}

//getTransactionByCore 获取交易单
func (wm *WalletManager) getTransactionByCore(txid string) (*Transaction, error) {

	var (
		result *gjson.Result
		err    error
	)

	request := []interface{}{
		txid,
		true,
	}

	result, err = wm.WalletClient.Call("gettransaction", request)
	if err != nil {

		request = []interface{}{
			txid,
			1,
		}

		result, err = wm.WalletClient.Call("gettransaction", request)
		if err != nil {
			return nil, err
		}
	}

	return wm.newTxByCore(result), nil
}

//GetTxOut 获取交易单输出信息，用于追溯交易单输入源头
func (wm *WalletManager) GetTxOut(txid string, vout uint64) (*Vout, error) {

	if wm.Config.RPCServerType == RPCServerExplorer {
		return wm.getTxOutByExplorer(txid, vout)
	} else {
		return wm.getTxOutByCore(txid, vout)
	}
}

//getTxOutByCore 获取交易单输出信息，用于追溯交易单输入源头
func (wm *WalletManager) getTxOutByCore(txid string, vout uint64) (*Vout, error) {

	request := []interface{}{
		txid,
		vout,
	}

	result, err := wm.WalletClient.Call("gettxout", request)
	if err != nil {
		return nil, err
	}

	output := newTxVoutByCore(result)

	/*
		{
			"bestblock": "0000000000012164c0fb1f7ac13462211aaaa83856073bf94faf2ea9c6ea193a",
			"confirmations": 8,
			"value": 0.64467249,
			"scriptPubKey": {
				"asm": "OP_DUP OP_HASH160 dbb494b649a48b22bfd6383dca1712cc401cddde OP_EQUALVERIFY OP_CHECKSIG",
				"hex": "76a914dbb494b649a48b22bfd6383dca1712cc401cddde88ac",
				"reqSigs": 1,
				"type": "pubkeyhash",
				"addresses": ["n1Yec3dmXEW4f8B5iJa5EsspNQ4Ar6K3Ek"]
			},
			"coinbase": false

		}
	*/

	return output, nil

}

//获取未扫记录
func (wm *WalletManager) GetUnscanRecords() ([]*UnscanRecord, error) {
	//获取本地区块高度
	db, err := storm.Open(filepath.Join(wm.Config.dbPath, wm.Config.BlockchainFile))
	if err != nil {
		return nil, err
	}
	defer db.Close()

	var list []*UnscanRecord
	err = db.All(&list)
	if err != nil {
		return nil, err
	}
	return list, nil
}

//DeleteUnscanRecord 删除指定高度的未扫记录
func (wm *WalletManager) DeleteUnscanRecord(height uint64) error {
	//获取本地区块高度
	db, err := storm.Open(filepath.Join(wm.Config.dbPath, wm.Config.BlockchainFile))
	if err != nil {
		return err
	}
	defer db.Close()

	var list []*UnscanRecord
	err = db.Find("BlockHeight", height, &list)
	if err != nil {
		return err
	}

	for _, r := range list {
		db.DeleteStruct(r)
	}

	return nil
}

//GetAssetsAccountBalanceByAddress 查询账户相关地址的交易记录
func (bs *HCBlockScanner) GetBalanceByAddress(address ...string) ([]*openwallet.Balance, error) {

	//if bs.wm.Config.RPCServerType != RPCServerExplorer {
	//	return nil, nil
	//}

	//addrsBalance := make([]*openwallet.Balance, 0)
	//
	//for _, a := range address {
	//	balance, err := bs.wm.getBalanceByExplorer(a)
	//	if err != nil {
	//		return nil, err
	//	}
	//
	//	addrsBalance = append(addrsBalance, balance)
	//}
	//
	//return addrsBalance, nil

	return bs.wm.getBalanceCalUnspent(address...)

}

//getBalanceByExplorer 获取地址余额
func (wm *WalletManager) getBalanceCalUnspent(address ...string) ([]*openwallet.Balance, error) {

	utxos, err := wm.ListUnspent(0, address...)
	if err != nil {
		return nil, err
	}

	addrBalanceMap := wm.calculateUnspent(utxos)
	addrBalanceArr := make([]*openwallet.Balance, 0)
	for _, a := range address {

		var obj *openwallet.Balance
		if b, exist := addrBalanceMap[a]; exist {
			obj = b
		} else {
			obj = &openwallet.Balance{
				Symbol:           wm.Symbol(),
				Address:          a,
				Balance:          "0",
				UnconfirmBalance: "0",
				ConfirmBalance:   "0",
			}
		}

		addrBalanceArr = append(addrBalanceArr, obj)
	}

	return addrBalanceArr, nil
}

//calculateUnspentByExplorer 通过未花计算余额
func (wm *WalletManager) calculateUnspent(utxos []*Unspent) map[string]*openwallet.Balance {

	addrBalanceMap := make(map[string]*openwallet.Balance)

	for _, utxo := range utxos {

		obj, exist := addrBalanceMap[utxo.Address]
		if !exist {
			obj = &openwallet.Balance{}
		}

		tu, _ := decimal.NewFromString(obj.UnconfirmBalance)
		tb, _ := decimal.NewFromString(obj.ConfirmBalance)

		if utxo.Spendable {
			if utxo.Confirmations > 0 {
				b, _ := decimal.NewFromString(utxo.Amount)
				tb = tb.Add(b)
			} else {
				u, _ := decimal.NewFromString(utxo.Amount)
				tu = tu.Add(u)
			}
		}

		obj.Symbol = wm.Symbol()
		obj.Address = utxo.Address
		obj.ConfirmBalance = tb.String()
		obj.UnconfirmBalance = tu.String()
		obj.Balance = tb.Add(tu).String()

		addrBalanceMap[utxo.Address] = obj
	}

	return addrBalanceMap

}
