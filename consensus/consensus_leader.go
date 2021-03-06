package consensus

import (
	"encoding/hex"
	"strconv"
	"time"

	"github.com/harmony-one/harmony/core"

	"github.com/ethereum/go-ethereum/rlp"
	protobuf "github.com/golang/protobuf/proto"
	"github.com/harmony-one/bls/ffi/go/bls"
	consensus_proto "github.com/harmony-one/harmony/api/consensus"
	"github.com/harmony-one/harmony/api/service/explorer"
	"github.com/harmony-one/harmony/core/types"
	bls_cosi "github.com/harmony-one/harmony/crypto/bls"
	"github.com/harmony-one/harmony/internal/profiler"
	"github.com/harmony-one/harmony/internal/utils"
	"github.com/harmony-one/harmony/p2p"
	"github.com/harmony-one/harmony/p2p/host"
)

const (
	waitForEnoughValidators = 1000
)

var (
	startTime time.Time
)

// WaitForNewBlock waits for the next new block to run consensus on
func (consensus *Consensus) WaitForNewBlock(blockChannel chan *types.Block, stopChan chan struct{}, stoppedChan chan struct{}) {
	go func() {
		defer close(stoppedChan)
		for {
			select {
			default:
				utils.GetLogInstance().Debug("Waiting for block", "consensus", consensus)
				// keep waiting for new blocks
				newBlock := <-blockChannel
				// TODO: think about potential race condition

				c := consensus.RemovePeers(consensus.OfflinePeerList)
				if c > 0 {
					utils.GetLogInstance().Debug("WaitForNewBlock", "removed peers", c)
				}

				for !consensus.HasEnoughValidators() {
					utils.GetLogInstance().Debug("Not enough validators", "# Validators", len(consensus.PublicKeys))
					time.Sleep(waitForEnoughValidators * time.Millisecond)
				}

				if core.IsEpochBlock(newBlock) {
					// Receive pRnd from DRG protocol
					utils.GetLogInstance().Debug("[DRG] Waiting for pRnd")
					pRndAndBitmap := <-consensus.PRndChannel
					utils.GetLogInstance().Debug("[DRG] GOT pRnd", "pRnd", pRndAndBitmap)
					pRnd := pRndAndBitmap[:32]
					bitmap := pRndAndBitmap[32:]
					vrfBitmap, _ := bls_cosi.NewMask(consensus.PublicKeys, consensus.leader.PubKey)
					vrfBitmap.SetMask(bitmap)

					// TODO: check validity of pRnd
					_ = pRnd
				}
				startTime = time.Now()
				utils.GetLogInstance().Debug("STARTING CONSENSUS", "numTxs", len(newBlock.Transactions()), "consensus", consensus, "startTime", startTime, "publicKeys", len(consensus.PublicKeys))
				for { // Wait until last consensus is finished
					if consensus.state == Finished {
						consensus.ResetState()
						consensus.startConsensus(newBlock)
						break
					}
					time.Sleep(500 * time.Millisecond)
				}
			case <-stopChan:
				return
			}
		}
	}()
}

// ProcessMessageLeader dispatches consensus message for the leader.
func (consensus *Consensus) ProcessMessageLeader(payload []byte) {
	message := consensus_proto.Message{}
	err := protobuf.Unmarshal(payload, &message)

	if err != nil {
		utils.GetLogInstance().Error("Failed to unmarshal message payload.", "err", err, "consensus", consensus)
	}

	switch message.Type {
	case consensus_proto.MessageType_PREPARE:
		consensus.processPrepareMessage(message)
	case consensus_proto.MessageType_COMMIT:
		consensus.processCommitMessage(message)
	default:
		utils.GetLogInstance().Error("Unexpected message type", "msgType", message.Type, "consensus", consensus)
	}
}

// startConsensus starts a new consensus for a block by broadcast a announce message to the validators
func (consensus *Consensus) startConsensus(newBlock *types.Block) {
	// Copy over block hash and block header data
	blockHash := newBlock.Hash()
	copy(consensus.blockHash[:], blockHash[:])

	utils.GetLogInstance().Debug("Start encoding block")
	// prepare message and broadcast to validators
	encodedBlock, err := rlp.EncodeToBytes(newBlock)
	if err != nil {
		utils.GetLogInstance().Debug("Failed encoding block")
		return
	}
	consensus.block = encodedBlock
	utils.GetLogInstance().Debug("Stop encoding block")

	msgToSend := consensus.constructAnnounceMessage()

	// Set state to AnnounceDone
	consensus.state = AnnounceDone

	// Leader sign the block hash itself
	consensus.prepareSigs[consensus.nodeID] = consensus.priKey.SignHash(consensus.blockHash[:])

	if utils.UseLibP2P {
		// Construct broadcast p2p message
		consensus.host.SendMessageToGroups([]p2p.GroupID{p2p.GroupIDBeacon}, host.ConstructP2pMessage(byte(17), msgToSend))
	} else {
		host.BroadcastMessageFromLeader(consensus.host, consensus.GetValidatorPeers(), msgToSend, consensus.OfflinePeers)
	}
}

// processPrepareMessage processes the prepare message sent from validators
func (consensus *Consensus) processPrepareMessage(message consensus_proto.Message) {
	validatorID := message.SenderId
	prepareSig := message.Payload

	prepareSigs := consensus.prepareSigs
	prepareBitmap := consensus.prepareBitmap

	consensus.mutex.Lock()
	defer consensus.mutex.Unlock()

	validatorPeer := consensus.getValidatorPeerByID(validatorID)

	if err := consensus.checkConsensusMessage(message, validatorPeer.PubKey); err != nil {
		utils.GetLogInstance().Debug("Failed to check the validator message", "validatorID", validatorID)
		return
	}

	// proceed only when the message is not received before
	_, ok := prepareSigs[validatorID]
	if ok {
		utils.GetLogInstance().Debug("Already received prepare message from the validator", "validatorID", validatorID)
		return
	}

	if len(prepareSigs) >= ((len(consensus.PublicKeys)*2)/3 + 1) {
		utils.GetLogInstance().Debug("Received additional prepare message", "validatorID", validatorID)
		return
	}

	// Check BLS signature for the multi-sig
	var sign bls.Sign
	err := sign.Deserialize(prepareSig)
	if err != nil {
		utils.GetLogInstance().Error("Failed to deserialize bls signature", "validatorID", validatorID)
		return
	}

	if !sign.VerifyHash(validatorPeer.PubKey, consensus.blockHash[:]) {
		utils.GetLogInstance().Error("Received invalid BLS signature", "validatorID", validatorID)
		return
	}

	utils.GetLogInstance().Debug("Received new prepare signature", "numReceivedSoFar", len(prepareSigs), "validatorID", validatorID, "PublicKeys", len(consensus.PublicKeys))
	prepareSigs[validatorID] = &sign
	prepareBitmap.SetKey(validatorPeer.PubKey, true) // Set the bitmap indicating that this validator signed.

	targetState := PreparedDone
	if len(prepareSigs) >= ((len(consensus.PublicKeys)*2)/3+1) && consensus.state < targetState {
		utils.GetLogInstance().Debug("Enough prepares received with signatures", "num", len(prepareSigs), "state", consensus.state)

		// Construct and broadcast prepared message
		msgToSend, aggSig := consensus.constructPreparedMessage()
		consensus.aggregatedPrepareSig = aggSig

		if utils.UseLibP2P {
			consensus.host.SendMessageToGroups([]p2p.GroupID{p2p.GroupIDBeacon}, host.ConstructP2pMessage(byte(17), msgToSend))
		} else {
			host.BroadcastMessageFromLeader(consensus.host, consensus.GetValidatorPeers(), msgToSend, consensus.OfflinePeers)
		}

		// Set state to targetState
		consensus.state = targetState

		// Leader sign the multi-sig and bitmap (for commit phase)
		multiSigAndBitmap := append(aggSig.Serialize(), prepareBitmap.Bitmap...)
		consensus.commitSigs[consensus.nodeID] = consensus.priKey.SignHash(multiSigAndBitmap)
	}
}

// Processes the commit message sent from validators
func (consensus *Consensus) processCommitMessage(message consensus_proto.Message) {
	validatorID := message.SenderId
	commitSig := message.Payload

	consensus.mutex.Lock()
	defer consensus.mutex.Unlock()

	validatorPeer := consensus.getValidatorPeerByID(validatorID)

	if err := consensus.checkConsensusMessage(message, validatorPeer.PubKey); err != nil {
		utils.GetLogInstance().Debug("Failed to check the validator message", "validatorID", validatorID)
		return
	}

	commitSigs := consensus.commitSigs
	commitBitmap := consensus.commitBitmap

	// proceed only when the message is not received before
	_, ok := commitSigs[validatorID]
	if ok {
		utils.GetLogInstance().Debug("Already received commit message from the validator", "validatorID", validatorID)
		return
	}

	if len((commitSigs)) >= ((len(consensus.PublicKeys)*2)/3 + 1) {
		utils.GetLogInstance().Debug("Received additional new commit message", "validatorID", strconv.Itoa(int(validatorID)))
		return
	}

	// Verify the signature on prepare multi-sig and bitmap is correct
	var sign bls.Sign
	err := sign.Deserialize(commitSig)
	if err != nil {
		utils.GetLogInstance().Debug("Failed to deserialize bls signature", "validatorID", validatorID)
		return
	}
	aggSig := bls_cosi.AggregateSig(consensus.GetPrepareSigsArray())
	if !sign.VerifyHash(validatorPeer.PubKey, append(aggSig.Serialize(), consensus.prepareBitmap.Bitmap...)) {
		utils.GetLogInstance().Error("Received invalid BLS signature", "validatorID", validatorID)
		return
	}

	utils.GetLogInstance().Debug("Received new commit message", "numReceivedSoFar", len(commitSigs), "validatorID", strconv.Itoa(int(validatorID)))
	commitSigs[validatorID] = &sign
	// Set the bitmap indicating that this validator signed.
	commitBitmap.SetKey(validatorPeer.PubKey, true)

	targetState := CommittedDone
	if len(commitSigs) >= ((len(consensus.PublicKeys)*2)/3+1) && consensus.state != targetState {
		utils.GetLogInstance().Info("Enough commits received!", "num", len(commitSigs), "state", consensus.state)

		// Construct and broadcast committed message
		msgToSend, aggSig := consensus.constructCommittedMessage()
		consensus.aggregatedCommitSig = aggSig

		if utils.UseLibP2P {
			consensus.host.SendMessageToGroups([]p2p.GroupID{p2p.GroupIDBeacon}, host.ConstructP2pMessage(byte(17), msgToSend))
		} else {
			host.BroadcastMessageFromLeader(consensus.host, consensus.GetValidatorPeers(), msgToSend, consensus.OfflinePeers)
		}

		var blockObj types.Block
		err := rlp.DecodeBytes(consensus.block, &blockObj)
		if err != nil {
			utils.GetLogInstance().Debug("failed to construct the new block after consensus")
		}

		// Sign the block
		copy(blockObj.Header().PrepareSignature[:], consensus.aggregatedPrepareSig.Serialize()[:])
		copy(blockObj.Header().PrepareBitmap[:], consensus.prepareBitmap.Bitmap)
		copy(blockObj.Header().CommitSignature[:], consensus.aggregatedCommitSig.Serialize()[:])
		copy(blockObj.Header().CommitBitmap[:], consensus.commitBitmap.Bitmap)

		consensus.state = targetState

		select {
		case consensus.VerifiedNewBlock <- &blockObj:
		default:
			utils.GetLogInstance().Info("[SYNC] consensus verified block send to chan failed", "blockHash", blockObj.Hash())
		}

		consensus.reportMetrics(blockObj)

		// Dump new block into level db.
		explorer.GetStorageInstance(consensus.leader.IP, consensus.leader.Port, true).Dump(&blockObj, consensus.consensusID)

		// Reset state to Finished, and clear other data.
		consensus.ResetState()
		consensus.consensusID++

		consensus.OnConsensusDone(&blockObj)
		utils.GetLogInstance().Debug("HOORAY!!! CONSENSUS REACHED!!!", "consensusID", consensus.consensusID, "numOfSignatures", len(commitSigs))

		// TODO: remove this temporary delay
		time.Sleep(500 * time.Millisecond)
		// Send signal to Node so the new block can be added and new round of consensus can be triggered
		consensus.ReadySignal <- struct{}{}
	}
}

func (consensus *Consensus) reportMetrics(block types.Block) {
	endTime := time.Now()
	timeElapsed := endTime.Sub(startTime)
	numOfTxs := len(block.Transactions())
	tps := float64(numOfTxs) / timeElapsed.Seconds()
	utils.GetLogInstance().Info("TPS Report",
		"numOfTXs", numOfTxs,
		"startTime", startTime,
		"endTime", endTime,
		"timeElapsed", timeElapsed,
		"TPS", tps,
		"consensus", consensus)

	// Post metrics
	profiler := profiler.GetProfiler()
	if profiler.MetricsReportURL == "" {
		return
	}

	txHashes := []string{}
	for i, end := 0, len(block.Transactions()); i < 3 && i < end; i++ {
		txHash := block.Transactions()[end-1-i].Hash()
		txHashes = append(txHashes, hex.EncodeToString(txHash[:]))
	}
	metrics := map[string]interface{}{
		"key":             hex.EncodeToString(consensus.pubKey.Serialize()),
		"tps":             tps,
		"txCount":         numOfTxs,
		"nodeCount":       len(consensus.PublicKeys) + 1,
		"latestBlockHash": hex.EncodeToString(consensus.blockHash[:]),
		"latestTxHashes":  txHashes,
		"blockLatency":    int(timeElapsed / time.Millisecond),
	}
	profiler.LogMetrics(metrics)
}

// HasEnoughValidators checks the number of publicKeys to determine
// if the shard has enough validators
// FIXME (HAR-82): we need epoch support or a better way to determine
// when to initiate the consensus
func (consensus *Consensus) HasEnoughValidators() bool {
	if len(consensus.PublicKeys) < consensus.MinPeers {
		return false
	}
	return true
}
