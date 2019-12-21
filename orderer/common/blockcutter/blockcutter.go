/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package blockcutter

import (
	"fmt"
	"github.com/hyperledger/fabric/common/channelconfig"
	cb "github.com/hyperledger/fabric/protos/common"

	"github.com/hyperledger/fabric/common/flogging"
	"github.com/op/go-logging"

	"github.com/golang/protobuf/proto"
	"github.com/hyperledger/fabric/core/ledger/kvledger/txmgmt/rwsetutil"
	"github.com/hyperledger/fabric/protos/utils"

	//"github.com/hyperledger/fabric/orderer/common/resolver"
	"github.com/khaoNEU/fabric-sigmod/orderer/common/resolver"

	"github.com/hyperledger/fabric/protos/ledger/rwset/kvrwset"
)

const pkgLogID = "orderer/common/blockcutter"

var logger *logging.Logger

func init() {
	logger = flogging.MustGetLogger(pkgLogID)
}

type OrdererConfigFetcher interface {
	OrdererConfig() (channelconfig.Orderer, bool)
}

// Receiver defines a sink for the ordered broadcast messages
// Receiver借口中定义了4个函数
type Receiver interface {
	// Ordered should be invoked sequentially as messages are ordered
	// Each batch in `messageBatches` will be wrapped into a block.
	// `pending` indicates if there are still messages pending in the receiver. It
	// is useful for Kafka orderer to determine the `LastOffsetPersisted` of block.
	Ordered(msg *cb.Envelope) (messageBatches [][]*cb.Envelope, pending bool)

	// Cut returns the current batch and starts a new one
	// Cut() 方法的功能主要是将超过给定最大字节数，就进行切割
	Cut() []*cb.Envelope

	// Process the transaction and record the read/write set into a bitset.
	// Used to resolve transactional dependencies within the batch.
	ProcessTransaction(msg *cb.Envelope) bool

	//Process the cross chain transaction (CCT) and record the read/write of the CCT int to a bitset
	//Used to resolve transactional CCT dependencies within the bath
	//自己定义的处理跨链事务的方法
	ProcessCrossChainTransaction(msg *cb.Envelope) bool

	// Process the current block and return (valid, invalid) two blocks.
	ProcessBlock() ([]*cb.Envelope, []*cb.Envelope)
}

type receiver struct {
	txCounter       int32
	maxMessageCount uint32
	maxUniqueKeys   uint32

	invalid       []bool
	keyVersionMap map[uint32]*kvrwset.Version
	keyTxMap      map[uint32][]int32

	txReadSet  [][]uint64
	txWriteSet [][]uint64

	uniqueKeyCounter uint32
	uniqueKeyMap     map[string]uint32

	initialized           bool
	pendingBatchSizeBytes uint32
	sharedConfigFetcher   OrdererConfigFetcher
	pendingBatch          []*cb.Envelope
}

// NewReceiverImpl creates a Receiver implementation based on the given configtxorderer manager
func NewReceiverImpl(sharedConfigFetcher OrdererConfigFetcher) Receiver {

	ordererConfig, ok := sharedConfigFetcher.OrdererConfig()

	if !ok {
		logger.Panicf("Could not retrieve orderer config to query batch parameters, block cutting is not possible")
	}

	batchSize := ordererConfig.BatchSize()

	return &receiver{
		txCounter:       0,
		maxMessageCount: batchSize.MaxMessageCount,
		maxUniqueKeys:   batchSize.MaxUniqueKeys, //MaxUniqueKeys值是configtx.yaml文件中配置的，定义了可以访问的key最大数

		txReadSet:  make([][]uint64, batchSize.MaxMessageCount),
		txWriteSet: make([][]uint64, batchSize.MaxMessageCount),

		invalid:       make([]bool, batchSize.MaxMessageCount),
		keyVersionMap: make(map[uint32]*kvrwset.Version),
		keyTxMap:      make(map[uint32][]int32),

		uniqueKeyCounter: 0,
		uniqueKeyMap:     make(map[string]uint32),

		sharedConfigFetcher: sharedConfigFetcher,
	}
}

/*
k1,k2,k3,k4,...,kn, t1,t2,t3,...,tm
k2,    _____       |
k3,   |     |      |   \          /
k4,   |     |      |    \        /
.     |     |      |     \  /\  /
kn,    -----       |      \/  \/
-------------------------------------
t1,   _____        |     _____
t2,  |     |       |    |     |
t3,  |     |       |    |     |
.    |_____|       |    |     |
.    |    \        |     -----
tm   |     \       |


R is stored in row-major form
W is stored in column major form

*/

// Add the read/write set to the R and W matrices
// returns true if the block must be cut due to increased key set size
// if a transaction contains more keys than MaxUniqueKeyCount, the batch is
// cut and this transaction is processed in a single transaction block. No
// serializability check is necessary for this transaction.
// ----------------------------------------------------------------------
// type Envelope struct {
//	// A marshaled Payload
//	Payload []byte `protobuf:"bytes,1,opt,name=payload,proto3" json:"payload,omitempty"`
//	// A signature by the creator specified in the Payload header
//	Signature []byte `protobuf:"bytes,2,opt,name=signature,proto3" json:"signature,omitempty"`
// }
// ---------------------------------------------------------------------
// 为receiver绑定ProcessTransaction方法
// 通过解析读写集来处理事务, Envelope中保存了数据和签名
//这个方法将每个事务tx拆分成对应的读写集
func (r *receiver) ProcessTransaction(msg *cb.Envelope) bool {
	// get current transaction id
	tid := r.txCounter

	data := make([]byte, messageSizeBytes(msg))

	var err error
	data, err = proto.Marshal(msg)

	//创建读写集，通过一维数组进行存储
	readSet := make([]uint64, r.maxUniqueKeys/64)
	writeSet := make([]uint64, r.maxUniqueKeys/64)

	if err == nil {
		//获取chaincode的action用来提取读写集
		resppayload, err := utils.GetActionFromEnvelope(data)

		if err == nil {
			txRWSet := &rwsetutil.TxRwSet{}
			if err = txRWSet.FromProtoBytes(resppayload.Results); err != nil {
				logger.Infof("from proto bytes error")
			} else {
				for _, ns := range txRWSet.NsRwSets[1:] {

					// generate key for each key in the read and write set and use it to insert the read/write key into RW matrices
					// 分析写集，解析出对哪些key进行了写操作
					for _, write := range ns.KvRwSet.Writes {
						writeKey := write.GetKey()

						// check if the key exists
						// 这里检查解析到的key是否已经存在，如果不存在则加入到uniqueKeyMap中
						key, ok := r.uniqueKeyMap[writeKey]

						if ok == false {

							//获取该写操作的from和to属性，表示从哪个channel写到哪个channel
							// from := write.GetFrom()
							fmt.Println("here we cal the GetFrom() function to get the transaction from filed")
							//to := write.GetTo()
							fmt.Println("here we cal the GetTo() function to get the transaction to filed")

							// if the key is not found, insert and increment
							// the key counter
							r.uniqueKeyMap[writeKey] = r.uniqueKeyCounter
							key = r.uniqueKeyCounter
							r.uniqueKeyCounter += 1
						}
						// set the respective bit in the writeSet

						if key >= r.maxUniqueKeys {
							// overflow of maxUniqueKeys
							// cut the block, and redo the work
							return true
						}

						index := key / 64
						writeSet[index] |= (uint64(1) << (key % 64))
					}

					//分析读集，解析出对哪些key进行读操作
					for _, read := range ns.KvRwSet.Reads {
						readKey := read.GetKey()
						readVer := read.GetVersion() //获得该读操作对应的版本号version
						key, ok := r.uniqueKeyMap[readKey]
						if ok == false {
							// if the key is not found, it is inserted. So increment
							// the key counter
							r.uniqueKeyMap[readKey] = r.uniqueKeyCounter
							key = r.uniqueKeyCounter
							r.uniqueKeyCounter += 1
						}

						ver, ok := r.keyVersionMap[key]
						if ok {
							if ver.BlockNum == readVer.BlockNum && ver.TxNum == readVer.TxNum {
								r.keyTxMap[key] = append(r.keyTxMap[key], tid)
							} else {
								for _, tx := range r.keyTxMap[key] {
									r.invalid[tx] = true
								}
								r.keyTxMap[key] = nil
							}
						} else {
							r.keyTxMap[key] = append(r.keyTxMap[key], tid)
							r.keyVersionMap[key] = readVer
						}

						// set the respective bit in the readSet
						if key >= r.maxUniqueKeys {
							// overflow of maxUniqueKeys
							// cut the block, and redo the work
							return true
						}

						index := key / 64
						readSet[index] |= (uint64(1) << (key % 64))
					}

				}

				// make sure the number of unique keys in the block will not overflow
			}
		} else {

			logger.Debug("resppayload error")
		}
	}

	//将上面解析的读写操作的key集合，放到对应事务的读写集合中
	r.txReadSet[tid] = readSet
	r.txWriteSet[tid] = writeSet
	r.txCounter += 1

	return false
}

//处理跨链事务CCT
func (r *receiver) ProcessCrossChainTransaction(msg *cb.Envelope) bool {
	fmt.Println("this is the ProcessCrossChainTransaction() function")
	//获取到事务的id
	txid := r.txCounter

	data := make([]byte, messageSizeBytes(msg))
	return false
}

// Ordered should be invoked sequentially as messages are ordered
//
// messageBatches length: 0, pending: false
//   - impossible, as we have just received a message
// messageBatches length: 0, pending: true
//   - no batch is cut and there are messages pending
// messageBatches length: 1, pending: false
//   - the message count reaches BatchSize.MaxMessageCount
// messageBatches length: 1, pending: true
//   - the current message will cause the pending batch size in bytes to exceed BatchSize.PreferredMaxBytes.
// messageBatches length: 2, pending: false
//   - the current message size in bytes exceeds BatchSize.PreferredMaxBytes, therefore isolated in its own batch.
// messageBatches length: 2, pending: true
//   - impossible
//
// Note that messageBatches can not be greater than 2.
func (r *receiver) Ordered(msg *cb.Envelope) (messageBatches [][]*cb.Envelope, pending bool) {
	ordererConfig, ok := r.sharedConfigFetcher.OrdererConfig()

	if !ok {
		logger.Panicf("Could not retrieve orderer config to query batch parameters, block cutting is not possible")
	}

	batchSize := ordererConfig.BatchSize()

	messageSizeBytes := messageSizeBytes(msg)
	if messageSizeBytes > batchSize.PreferredMaxBytes {
		logger.Debugf("The current message, with %v bytes, is larger than the preferred batch size of %v bytes and will be isolated.", messageSizeBytes, batchSize.PreferredMaxBytes)

		// cut pending batch, if it has any messages
		if len(r.pendingBatch) > 0 {
			messageBatch := r.Cut()
			messageBatches = append(messageBatches, messageBatch)
		}

		// create new batch with single message
		messageBatches = append(messageBatches, []*cb.Envelope{msg})

		return
	}

	messageWillOverflowBatchSizeBytes := r.pendingBatchSizeBytes+messageSizeBytes > batchSize.PreferredMaxBytes

	if messageWillOverflowBatchSizeBytes {
		logger.Debugf("The current message, with %v bytes, will overflow the pending batch of %v bytes.", messageSizeBytes, r.pendingBatchSizeBytes)
		logger.Debugf("Pending batch would overflow if current message is added, cutting batch now.")
		messageBatch := r.Cut()
		messageBatches = append(messageBatches, messageBatch)
	}

	logger.Debugf("Enqueuing message into batch")

	maxUniqueKeyCut := r.ProcessTransaction(msg)

	if maxUniqueKeyCut && len(r.pendingBatch) > 0 {
		logger.Debugf("Overflow of MaxUniqueKeyCount, cutting block now")
		messageBatch := r.Cut()
		messageBatches = append(messageBatches, messageBatch)
		maxUniqueKeyCut = r.ProcessTransaction(msg)
	}

	if maxUniqueKeyCut {
		// This transaction has more keys than maxUniqueKeyCount
		// create a batch with single transaction
		messageBatches = append(messageBatches, []*cb.Envelope{msg})
		logger.Debugf("Single transaction overflows the MaxUniqueKeyCount")
		return
	}

	r.pendingBatch = append(r.pendingBatch, msg)
	r.pendingBatchSizeBytes += messageSizeBytes
	pending = true

	if uint32(len(r.pendingBatch)) >= batchSize.MaxMessageCount {
		logger.Debugf("Batch size met, cutting batch")
		messageBatch := r.Cut()
		messageBatches = append(messageBatches, messageBatch)
		pending = false
	}

	return
}

// Cut returns the current batch and starts a new one
func (r *receiver) Cut() []*cb.Envelope {

	validBatch, _ := r.ProcessBlock()

	r.pendingBatch = nil
	r.pendingBatchSizeBytes = 0
	r.txCounter = 0
	r.uniqueKeyCounter = 0
	r.uniqueKeyMap = nil
	r.uniqueKeyMap = make(map[string]uint32)

	r.txReadSet = make([][]uint64, r.maxMessageCount)
	r.txWriteSet = make([][]uint64, r.maxMessageCount)

	r.invalid = make([]bool, r.maxMessageCount)
	r.keyVersionMap = make(map[uint32]*kvrwset.Version)
	r.keyTxMap = make(map[uint32][]int32)

	return validBatch
}

// Process the block and partition it into two blocks
// containing serialized transactions, and invalid transactions.
//处理区块
func (r *receiver) ProcessBlock() ([]*cb.Envelope, []*cb.Envelope) {
	if len(r.pendingBatch) > 1 {
		graph := make([][]int32, r.txCounter)
		invgraph := make([][]int32, r.txCounter)
		//有txCounter个事务，就对应了2*txCounter个graph
		for i := int32(0); i < r.txCounter; i++ {
			graph[i] = make([]int32, 0, r.txCounter)
			invgraph[i] = make([]int32, 0, r.txCounter)
		}

		// for every transactions, find the intersection between the readSet and the writeSet
		//对于每个事务，计算读写集之间的交集
		for i := int32(0); i < r.txCounter; i++ {
			for j := int32(0); j < r.txCounter; j++ {
				//相同事务，或者事务不合法时，继续
				if i == j || r.invalid[i] || r.invalid[j] {
					continue
				} else {
					for k := uint32(0); k < (r.maxUniqueKeys / 64); k++ {
						if (r.txWriteSet[i][k] & r.txReadSet[j][k]) != 0 {
							//更新图
							graph[i] = append(graph[i], j)
							invgraph[j] = append(invgraph[j], i)
							break
						}
					}
				}
			}
		}

		r.txWriteSet = nil
		r.txReadSet = nil

		//调用resolver中的方法，把上面创建好的图作为参数传递
		resGen := resolver.NewResolver(&graph, &invgraph)

		//调用GetSchedule()方法获得串行序列
		res, _ := resGen.GetSchedule()
		lenres := len(res)

		resGen = nil
		graph = nil
		invgraph = nil

		validBatch := make([]*cb.Envelope, lenres)

		for i := 0; i < lenres; i++ {
			validBatch[i] = r.pendingBatch[res[(lenres-1)-i]]
		}

		// log some information
		//日志中记录序列信息
		logger.Debugf("schedule-> %v", res)
		logger.Infof("oldBlockSize:%d, newBlockSize:%d", len(r.pendingBatch), len(validBatch))

		return validBatch, nil
	} else {
		return r.pendingBatch, nil
	}
}

func messageSizeBytes(message *cb.Envelope) uint32 {
	return uint32(len(message.Payload) + len(message.Signature))
}
