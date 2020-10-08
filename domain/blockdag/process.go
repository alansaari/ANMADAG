package blockdag

import (
	"fmt"
	"time"

	"github.com/kaspanet/kaspad/infrastructure/db/dbaccess"
	"github.com/kaspanet/kaspad/util"
	"github.com/kaspanet/kaspad/util/daghash"
	"github.com/pkg/errors"
)

// selectedParentChainUpdates represents the updates made to the selected parent chain after
// a block had been added to the DAG.
type selectedParentChainUpdates struct {
	removedChainBlockHashes []*daghash.Hash
	addedChainBlockHashes   []*daghash.Hash
}

// ProcessBlock is the main workhorse for handling insertion of new blocks into
// the block DAG. It includes functionality such as rejecting duplicate
// blocks, ensuring blocks follow all rules, orphan handling, and insertion into
// the block DAG.
//
// This function is safe for concurrent access.
func (dag *BlockDAG) ProcessBlock(block *util.Block, flags BehaviorFlags) (isOrphan bool, isDelayed bool, err error) {
	dag.dagLock.Lock()
	defer dag.dagLock.Unlock()
	return dag.processBlockNoLock(block, flags)
}

func (dag *BlockDAG) processBlockNoLock(block *util.Block, flags BehaviorFlags) (isOrphan bool, isDelayed bool, err error) {
	blockHash := block.Hash()
	log.Tracef("Processing block %s", blockHash)

	err = dag.checkDuplicateBlock(blockHash, flags)
	if err != nil {
		return false, false, err
	}

	err = dag.checkBlockSanity(block, flags)
	if err != nil {
		return false, false, err
	}

	isOrphan, isDelayed, err = dag.checkDelayedAndOrphanBlocks(block, flags)
	if isOrphan || isDelayed || err != nil {
		return isOrphan, isDelayed, err
	}

	err = dag.maybeAcceptBlock(block, flags)
	if err != nil {
		return false, false, err
	}

	err = dag.processOrphansAndDelayedBlocks(blockHash, flags)
	if err != nil {
		return false, false, err
	}

	return false, false, nil
}

func (dag *BlockDAG) checkDelayedAndOrphanBlocks(block *util.Block, flags BehaviorFlags) (isOrphan bool, isDelayed bool, err error) {
	if !isBehaviorFlagRaised(flags, BFAfterDelay) {
		isDelayed, err := dag.checkBlockDelay(block, flags)
		if err != nil {
			return false, false, err
		}
		if isDelayed {
			return false, true, nil
		}
	}
	return dag.checkMissingParents(block, flags)
}

func (dag *BlockDAG) checkBlockDelay(block *util.Block, flags BehaviorFlags) (isDelayed bool, err error) {
	delay, isDelayed := dag.shouldBlockBeDelayed(block)
	if isDelayed && isBehaviorFlagRaised(flags, BFDisallowDelay) {
		str := fmt.Sprintf("cannot process blocks beyond the "+
			"allowed time offset while the BFDisallowDelay flag is "+
			"raised %s", block.Hash())
		return false, ruleError(ErrDelayedBlockIsNotAllowed, str)
	}

	if isDelayed {
		err := dag.addDelayedBlock(block, flags, delay)
		if err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func (dag *BlockDAG) checkMissingParents(block *util.Block, flags BehaviorFlags) (isOrphan bool, isDelayed bool, err error) {
	var missingParents []*daghash.Hash
	for _, parentHash := range block.MsgBlock().Header.ParentHashes {
		if !dag.IsInDAG(parentHash) {
			missingParents = append(missingParents, parentHash)
		}
	}

	if len(missingParents) > 0 && isBehaviorFlagRaised(flags, BFDisallowOrphans) {
		str := fmt.Sprintf("cannot process orphan blocks while the "+
			"BFDisallowOrphans flag is raised %s", block.Hash())
		return false, false, ruleError(ErrOrphanBlockIsNotAllowed, str)
	}

	// Handle the case of a block with a valid timestamp(non-delayed) which points to a delayed block.
	delay, isParentDelayed := dag.maxDelayOfParents(missingParents)
	if isParentDelayed {
		// Add Millisecond to ensure that parent process time will be after its child.
		delay += time.Millisecond
		err := dag.addDelayedBlock(block, flags, delay)
		if err != nil {
			return false, false, err
		}
		return false, true, nil
	}

	// Handle orphan blocks.
	if len(missingParents) > 0 {
		dag.addOrphanBlock(block)
		return true, false, nil
	}
	return false, false, nil
}

func (dag *BlockDAG) processOrphansAndDelayedBlocks(blockHash *daghash.Hash, flags BehaviorFlags) error {
	err := dag.processOrphans(blockHash, flags)
	if err != nil {
		return err
	}

	if !isBehaviorFlagRaised(flags, BFAfterDelay) {
		err = dag.processDelayedBlocks()
		if err != nil {
			return err
		}
	}
	return nil
}

// maybeAcceptBlock potentially accepts a block into the block DAG. It
// performs several validation checks which depend on its position within
// the block DAG before adding it. The block is expected to have already
// gone through ProcessBlock before calling this function with it.
//
// The flags are also passed to checkBlockContext and connectBlock. See
// their documentation for how the flags modify their behavior.
//
// This function MUST be called with the dagLock held (for writes).
func (dag *BlockDAG) maybeAcceptBlock(block *util.Block, flags BehaviorFlags) error {
	err := dag.checkBlockContext(block, flags)
	if err != nil {
		return err
	}

	newNode, selectedParentAnticone, err := dag.createBlockNodeFromBlock(block)
	if err != nil {
		return err
	}

	chainUpdates, err := dag.connectBlock(newNode, block, selectedParentAnticone, flags)
	if err != nil {
		return dag.handleConnectBlockError(err, newNode)
	}

	dag.notifyBlockAccepted(block, chainUpdates, flags)

	log.Debugf("Accepted block %s with status '%s'", newNode.hash, dag.index.BlockNodeStatus(newNode))

	return nil
}

// createBlockNodeFromBlock generates a new block node for the given block
// and stores it in the block index with statusDataStored.
func (dag *BlockDAG) createBlockNodeFromBlock(block *util.Block) (
	newNode *blockNode, selectedParentAnticone []*blockNode, err error) {

	// Create a new block node for the block and add it to the node index.
	parents, err := lookupParentNodes(block, dag)
	if err != nil {
		return nil, nil, err
	}
	newNode, selectedParentAnticone = dag.newBlockNode(&block.MsgBlock().Header, parents)
	dag.index.AddNode(newNode)
	dag.index.SetBlockNodeStatus(newNode, statusDataStored)

	// Insert the block into the database if it's not already there. Even
	// though it is possible the block will ultimately fail to connect, it
	// has already passed all proof-of-work and validity tests which means
	// it would be prohibitively expensive for an attacker to fill up the
	// disk with a bunch of blocks that fail to connect. This is necessary
	// since it allows block download to be decoupled from the much more
	// expensive connection logic. It also has some other nice properties
	// such as making blocks that never become part of the DAG or
	// blocks that fail to connect available for further analysis.
	dbTx, err := dag.databaseContext.NewTx()
	if err != nil {
		return nil, nil, err
	}
	defer dbTx.RollbackUnlessClosed()
	blockExists, err := dbaccess.HasBlock(dbTx, block.Hash())
	if err != nil {
		return nil, nil, err
	}
	if !blockExists {
		err := storeBlock(dbTx, block)
		if err != nil {
			return nil, nil, err
		}
	}
	err = dag.index.flushToDB(dbTx)
	if err != nil {
		return nil, nil, err
	}
	err = dbTx.Commit()
	if err != nil {
		return nil, nil, err
	}
	return newNode, selectedParentAnticone, nil
}

// connectBlock handles connecting the passed node/block to the DAG.
//
// This function MUST be called with the DAG state lock held (for writes).
func (dag *BlockDAG) connectBlock(newNode *blockNode,
	block *util.Block, selectedParentAnticone []*blockNode, flags BehaviorFlags) (*selectedParentChainUpdates, error) {

	err := newNode.checkDAGRelations()
	if err != nil {
		return nil, err
	}

	err = dag.checkBlockTransactionsFinalized(block, newNode, flags)
	if err != nil {
		return nil, err
	}

	err = dag.checkBlockHasNoChainedTransactions(block, newNode, flags)
	if err != nil {
		return nil, err
	}

	if err := dag.validateGasLimit(block); err != nil {
		return nil, err
	}

	isNewSelectedTip := dag.isNewSelectedTip(newNode)
	if !isNewSelectedTip {
		dag.index.SetBlockNodeStatus(newNode, statusUTXOPendingVerification)
	}

	dbTx, err := dag.databaseContext.NewTx()
	if err != nil {
		return nil, err
	}
	defer dbTx.RollbackUnlessClosed()

	if isNewSelectedTip {
		err = dag.resolveNodeStatus(newNode, dbTx)
		if err != nil {
			return nil, err
		}
		if dag.index.BlockNodeStatus(newNode) == statusValid {
			isViolatingFinality, err := newNode.isViolatingFinality()
			if err != nil {
				return nil, err
			}
			if isViolatingFinality {
				dag.index.SetBlockNodeStatus(newNode, statusUTXOPendingVerification)
				dag.sendNotification(NTFinalityConflict, &FinalityConflictNotificationData{
					ViolatingBlockHash: newNode.hash,
				})
			}
		}
	}

	chainUpdates, err := dag.applyDAGChanges(newNode, selectedParentAnticone, dbTx)
	if err != nil {
		return nil, err
	}

	err = dag.saveChangesFromBlock(block, dbTx)
	if err != nil {
		return nil, err
	}

	err = dbTx.Commit()
	if err != nil {
		return nil, err
	}

	dag.clearDirtyEntries()

	dag.addBlockProcessingTimestamp()
	dag.blockCount++

	return chainUpdates, nil
}

// isNewSelectedTip determines if a new blockNode qualifies to be the next selectedTip
func (dag *BlockDAG) isNewSelectedTip(newNode *blockNode) bool {
	return newNode.isGenesis() || dag.selectedTip().less(newNode)
}

func (dag *BlockDAG) updateVirtualAndTips(node *blockNode, dbTx *dbaccess.TxContext) (*selectedParentChainUpdates, error) {
	didVirtualParentsChange, chainUpdates, err := dag.addTip(node)
	if err != nil {
		return nil, err
	}

	if didVirtualParentsChange {
		// Build a UTXO set for the new virtual block
		newVirtualUTXO, _, _, err := dag.pastUTXO(dag.virtual.blockNode)
		if err != nil {
			return nil, errors.Wrap(err, "could not restore past UTXO for virtual")
		}

		// Apply new utxoDiffs to all the tips
		err = updateValidTipsUTXO(dag, newVirtualUTXO)
		if err != nil {
			return nil, errors.Wrap(err, "failed updating the tips' UTXO")
		}

		// It is now safe to meld the UTXO set to base.
		diffSet := newVirtualUTXO.(*DiffUTXOSet)
		virtualUTXODiff := diffSet.UTXODiff
		err = dag.meldVirtualUTXO(diffSet)
		if err != nil {
			return nil, errors.Wrap(err, "failed melding the virtual UTXO")
		}

		// Update the UTXO set using the diffSet that was melded into the
		// full UTXO set.
		err = updateUTXOSet(dbTx, virtualUTXODiff)
		if err != nil {
			return nil, err
		}
	}
	return chainUpdates, nil
}

func (dag *BlockDAG) validateAndApplyUTXOSet(
	node *blockNode, block *util.Block, dbTx *dbaccess.TxContext) error {

	if !node.isGenesis() {
		err := dag.resolveNodeStatus(node.selectedParent, dbTx)
		if err != nil {
			return err
		}

		if dag.index.BlockNodeStatus(node.selectedParent) == statusDisqualifiedFromChain {
			return ruleError(ErrSelectedParentDisqualifiedFromChain,
				"Block's selected parent is disqualified from chain")
		}
	}

	utxoVerificationData, err := node.verifyAndBuildUTXO(block.Transactions())
	if err != nil {
		return errors.Wrapf(err, "error verifying UTXO for %s", node)
	}

	err = node.validateCoinbaseTransaction(dag, block, utxoVerificationData.txsAcceptanceData)
	if err != nil {
		return err
	}

	err = dag.applyUTXOSetChanges(node, utxoVerificationData, dbTx)
	if err != nil {
		return err
	}

	return nil
}

func (dag *BlockDAG) applyUTXOSetChanges(
	node *blockNode, utxoVerificationData *utxoVerificationOutput, dbTx *dbaccess.TxContext) error {

	dag.index.SetBlockNodeStatus(node, statusValid)

	if !node.hasValidChildren() {
		err := dag.addValidTip(node)
		if err != nil {
			return err
		}
	}

	dag.multisetStore.setMultiset(node, utxoVerificationData.newBlockMultiset)

	err := node.updateDiffAndDiffChild(utxoVerificationData.newBlockPastUTXO)
	if err != nil {
		return err
	}

	if err := node.updateParentsDiffs(dag, utxoVerificationData.newBlockPastUTXO); err != nil {
		return errors.Wrapf(err, "failed updating parents of %s", node)
	}

	if dag.indexManager != nil {
		err := dag.indexManager.ConnectBlock(dbTx, node.hash, utxoVerificationData.txsAcceptanceData)
		if err != nil {
			return err
		}
	}

	return nil
}

func (dag *BlockDAG) resolveNodeStatus(node *blockNode, dbTx *dbaccess.TxContext) error {
	blockStatus := dag.index.BlockNodeStatus(node)
	if blockStatus != statusValid && blockStatus != statusDisqualifiedFromChain {
		block, err := dag.fetchBlockByHash(node.hash)
		if err != nil {
			return err
		}

		err = dag.validateAndApplyUTXOSet(node, block, dbTx)
		if err != nil {
			if !errors.As(err, &(RuleError{})) {
				return err
			}
			dag.index.SetBlockNodeStatus(node, statusDisqualifiedFromChain)
		}
	}
	return nil
}

func (dag *BlockDAG) resolveNodeStatusInNewTransaction(node *blockNode) error {
	dbTx, err := dag.databaseContext.NewTx()
	if err != nil {
		return err
	}
	defer dbTx.RollbackUnlessClosed()
	err = dag.resolveNodeStatus(node, dbTx)
	if err != nil {
		return err
	}
	err = dbTx.Commit()
	if err != nil {
		return err
	}
	return nil
}

func (dag *BlockDAG) applyDAGChanges(node *blockNode, selectedParentAnticone []*blockNode, dbTx *dbaccess.TxContext) (
	*selectedParentChainUpdates, error) {

	// Add the block to the reachability tree
	err := dag.reachabilityTree.addBlock(node, selectedParentAnticone)
	if err != nil {
		return nil, errors.Wrap(err, "failed adding block to the reachability tree")
	}

	node.updateParentsChildren()

	chainUpdates, err := dag.updateVirtualAndTips(node, dbTx)
	if err != nil {
		return nil, err
	}

	return chainUpdates, nil
}

func (dag *BlockDAG) saveChangesFromBlock(block *util.Block, dbTx *dbaccess.TxContext) error {
	err := dag.index.flushToDB(dbTx)
	if err != nil {
		return err
	}

	err = dag.utxoDiffStore.flushToDB(dbTx)
	if err != nil {
		return err
	}

	err = dag.reachabilityTree.storeState(dbTx)
	if err != nil {
		return err
	}

	err = dag.multisetStore.flushToDB(dbTx)
	if err != nil {
		return err
	}

	// Update DAG state.
	err = dag.saveState(dbTx)
	if err != nil {
		return err
	}

	// Scan all accepted transactions and register any subnetwork registry
	// transaction. If any subnetwork registry transaction is not well-formed,
	// fail the entire block.
	err = registerSubnetworks(dbTx, block.Transactions())
	if err != nil {
		return err
	}

	return nil
}

// boundedMergeBreakingParents returns all parents of given `node` that break the bounded merge depth rule:
// All blocks in node.MergeSet should be in future of node.finalityPoint, with the following exception:
// If there exists a block C violating this, i.e., C is in node's merge set and node.finalityPoint's anticone,
// then there must be a "kosherizing" block D in C's Future such that D is in node.blues
// and node.finalityPoint is in D.SelectedChain
func (dag *BlockDAG) boundedMergeBreakingParents(node *blockNode) (blockSet, error) {
	potentiallyKosherizingBlocks, err := node.nonBoundedMergeDepthViolatingBlues()
	if err != nil {
		return nil, err
	}
	badReds := []*blockNode{}

	finalityPoint := node.finalityPoint()
	for _, redBlock := range node.reds {
		isFinalityPointInPast, err := dag.isInPast(finalityPoint, redBlock)
		if err != nil {
			return nil, err
		}
		if isFinalityPointInPast {
			continue
		}

		isKosherized := false
		for potentiallyKosherizingBlock := range potentiallyKosherizingBlocks {
			isKosherized, err = dag.isInPast(redBlock, potentiallyKosherizingBlock)
			if err != nil {
				return nil, err
			}
			if isKosherized {
				break
			}
		}

		if !isKosherized {
			badReds = append(badReds, redBlock)
		}
	}

	boundedMergeBreakingParents := newBlockSet()
	for parent := range node.parents {
		isBadRedInPast := false
		for _, badRedBlock := range badReds {
			isBadRedInPast, err = dag.isInPast(badRedBlock, parent)
			if err != nil {
				return nil, err
			}
			if isBadRedInPast {
				break
			}
		}

		if isBadRedInPast {
			boundedMergeBreakingParents.add(parent)
		}
	}
	return boundedMergeBreakingParents, nil
}

func (dag *BlockDAG) clearDirtyEntries() {
	dag.index.clearDirtyEntries()
	dag.utxoDiffStore.clearDirtyEntries()
	dag.utxoDiffStore.clearOldEntries()
	dag.reachabilityTree.store.clearDirtyEntries()
	dag.multisetStore.clearNewEntries()
}

func (dag *BlockDAG) handleConnectBlockError(err error, newNode *blockNode) error {
	if errors.As(err, &RuleError{}) {
		dag.index.SetBlockNodeStatus(newNode, statusValidateFailed)

		dbTx, err := dag.databaseContext.NewTx()
		if err != nil {
			return err
		}
		defer dbTx.RollbackUnlessClosed()

		err = dag.index.flushToDB(dbTx)
		if err != nil {
			return err
		}
		err = dbTx.Commit()
		if err != nil {
			return err
		}
	}
	return err
}

// notifyBlockAccepted notifies the caller that the new block was
// accepted into the block DAG. The caller would typically want to
// react by relaying the inventory to other peers.
func (dag *BlockDAG) notifyBlockAccepted(block *util.Block, chainUpdates *selectedParentChainUpdates, flags BehaviorFlags) {
	dag.sendNotification(NTBlockAdded, &BlockAddedNotificationData{
		Block:         block,
		WasUnorphaned: flags&BFWasUnorphaned != 0,
	})
	if len(chainUpdates.addedChainBlockHashes) > 0 {
		dag.sendNotification(NTChainChanged, &ChainChangedNotificationData{
			RemovedChainBlockHashes: chainUpdates.removedChainBlockHashes,
			AddedChainBlockHashes:   chainUpdates.addedChainBlockHashes,
		})
	}
}