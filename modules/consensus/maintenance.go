package consensus

import (
	"errors"

	"github.com/turtledex/bolt"

	"github.com/turtledex/TurtleDexCore/build"
	"github.com/turtledex/TurtleDexCore/modules"
	"github.com/turtledex/TurtleDexCore/types"
	"github.com/turtledex/encoding"
)

var (
	errOutputAlreadyMature = errors.New("delayed ttdc output is already in the matured outputs set")
	errPayoutsAlreadyPaid  = errors.New("payouts are already in the consensus set")
	errStorageProofTiming  = errors.New("missed proof triggered for file contract that is not expiring")
)

// applyFoundationSubsidy adds a Foundation subsidy to the consensus set as a
// delayed ttdc output. If no subsidy is due on the given block, no output is
// added.
func applyFoundationSubsidy(tx *bolt.Tx, pb *processedBlock) {
	// NOTE: this conditional is split up to better visualize test coverage
	if pb.Height < types.FoundationHardforkHeight {
		return
	} else if (pb.Height-types.FoundationHardforkHeight)%types.FoundationSubsidyFrequency != 0 {
		return
	}
	value := types.FoundationSubsidyPerBlock.Mul64(uint64(types.FoundationSubsidyFrequency))
	if pb.Height == types.FoundationHardforkHeight {
		value = types.InitialFoundationSubsidy
	}
	// The subsidy is always sent to the primary address.
	addr, _ := getFoundationUnlockHashes(tx)
	dscod := modules.DelayedTurtleDexcoinOutputDiff{
		Direction: modules.DiffApply,
		ID:        pb.Block.ID().FoundationSubsidyID(),
		TurtleDexcoinOutput: types.TurtleDexcoinOutput{
			Value:      value,
			UnlockHash: addr,
		},
		MaturityHeight: pb.Height + types.MaturityDelay,
	}
	pb.DelayedTurtleDexcoinOutputDiffs = append(pb.DelayedTurtleDexcoinOutputDiffs, dscod)
	commitDelayedTurtleDexcoinOutputDiff(tx, dscod, modules.DiffApply)
}

// applyMinerPayouts adds a block's miner payouts to the consensus set as
// delayed ttdc outputs.
func applyMinerPayouts(tx *bolt.Tx, pb *processedBlock) {
	for i := range pb.Block.MinerPayouts {
		mpid := pb.Block.MinerPayoutID(uint64(i))
		dscod := modules.DelayedTurtleDexcoinOutputDiff{
			Direction:      modules.DiffApply,
			ID:             mpid,
			TurtleDexcoinOutput:  pb.Block.MinerPayouts[i],
			MaturityHeight: pb.Height + types.MaturityDelay,
		}
		pb.DelayedTurtleDexcoinOutputDiffs = append(pb.DelayedTurtleDexcoinOutputDiffs, dscod)
		commitDelayedTurtleDexcoinOutputDiff(tx, dscod, modules.DiffApply)
	}
}

// applyMaturedTurtleDexcoinOutputs goes through the list of ttdc outputs that
// have matured and adds them to the consensus set. This also updates the block
// node diff set.
func applyMaturedTurtleDexcoinOutputs(tx *bolt.Tx, pb *processedBlock) {
	// Skip this step if the blockchain is not old enough to have maturing
	// outputs.
	if pb.Height < types.MaturityDelay {
		return
	}

	// Iterate through the list of delayed ttdc outputs. Sometimes boltdb
	// has trouble if you delete elements in a bucket while iterating through
	// the bucket (and sometimes not - nondeterministic), so all of the
	// elements are collected into an array and then deleted after the bucket
	// scan is complete.
	bucketID := append(prefixDSCO, encoding.Marshal(pb.Height)...)
	var scods []modules.TurtleDexcoinOutputDiff
	var dscods []modules.DelayedTurtleDexcoinOutputDiff
	dbErr := tx.Bucket(bucketID).ForEach(func(idBytes, scoBytes []byte) error {
		// Decode the key-value pair into an id and a ttdc output.
		var id types.TurtleDexcoinOutputID
		var sco types.TurtleDexcoinOutput
		copy(id[:], idBytes)
		encErr := encoding.Unmarshal(scoBytes, &sco)
		if build.DEBUG && encErr != nil {
			panic(encErr)
		}

		// Sanity check - the output should not already be in ttdcOuptuts.
		if build.DEBUG && isTurtleDexcoinOutput(tx, id) {
			panic(errOutputAlreadyMature)
		}

		// Add the output to the ConsensusSet and record the diff in the
		// blockNode.
		scod := modules.TurtleDexcoinOutputDiff{
			Direction:     modules.DiffApply,
			ID:            id,
			TurtleDexcoinOutput: sco,
		}
		scods = append(scods, scod)

		// Create the dscod and add it to the list of dscods that should be
		// deleted.
		dscod := modules.DelayedTurtleDexcoinOutputDiff{
			Direction:      modules.DiffRevert,
			ID:             id,
			TurtleDexcoinOutput:  sco,
			MaturityHeight: pb.Height,
		}
		dscods = append(dscods, dscod)
		return nil
	})
	if build.DEBUG && dbErr != nil {
		panic(dbErr)
	}
	for _, scod := range scods {
		pb.TurtleDexcoinOutputDiffs = append(pb.TurtleDexcoinOutputDiffs, scod)
		commitTurtleDexcoinOutputDiff(tx, scod, modules.DiffApply)
	}
	for _, dscod := range dscods {
		pb.DelayedTurtleDexcoinOutputDiffs = append(pb.DelayedTurtleDexcoinOutputDiffs, dscod)
		commitDelayedTurtleDexcoinOutputDiff(tx, dscod, modules.DiffApply)
	}
	deleteDSCOBucket(tx, pb.Height)
}

// applyMissedStorageProof adds the outputs and diffs that result from a file
// contract expiring.
func applyMissedStorageProof(tx *bolt.Tx, pb *processedBlock, fcid types.FileContractID) (dscods []modules.DelayedTurtleDexcoinOutputDiff, fcd modules.FileContractDiff) {
	// Sanity checks.
	fc, err := getFileContract(tx, fcid)
	if build.DEBUG && err != nil {
		panic(err)
	}
	if build.DEBUG {
		// Check that the file contract in question expires at pb.Height.
		if fc.WindowEnd != pb.Height {
			panic(errStorageProofTiming)
		}
	}

	// Add all of the outputs in the missed proof outputs to the consensus set.
	for i, mpo := range fc.MissedProofOutputs {
		// Sanity check - output should not already exist.
		spoid := fcid.StorageProofOutputID(types.ProofMissed, uint64(i))
		if build.DEBUG && isTurtleDexcoinOutput(tx, spoid) {
			panic(errPayoutsAlreadyPaid)
		}

		// Don't add the output if the value is zero.
		dscod := modules.DelayedTurtleDexcoinOutputDiff{
			Direction:      modules.DiffApply,
			ID:             spoid,
			TurtleDexcoinOutput:  mpo,
			MaturityHeight: pb.Height + types.MaturityDelay,
		}
		dscods = append(dscods, dscod)
	}

	// Remove the file contract from the consensus set and record the diff in
	// the blockNode.
	fcd = modules.FileContractDiff{
		Direction:    modules.DiffRevert,
		ID:           fcid,
		FileContract: fc,
	}
	return dscods, fcd
}

// applyFileContractMaintenance looks for all of the file contracts that have
// expired without an appropriate storage proof, and calls 'applyMissedProof'
// for the file contract.
func applyFileContractMaintenance(tx *bolt.Tx, pb *processedBlock) {
	// Get the bucket pointing to all of the expiring file contracts.
	fceBucketID := append(prefixFCEX, encoding.Marshal(pb.Height)...)
	fceBucket := tx.Bucket(fceBucketID)
	// Finish if there are no expiring file contracts.
	if fceBucket == nil {
		return
	}

	var dscods []modules.DelayedTurtleDexcoinOutputDiff
	var fcds []modules.FileContractDiff
	err := fceBucket.ForEach(func(keyBytes, valBytes []byte) error {
		var id types.FileContractID
		copy(id[:], keyBytes)
		amspDSCODS, fcd := applyMissedStorageProof(tx, pb, id)
		fcds = append(fcds, fcd)
		dscods = append(dscods, amspDSCODS...)
		return nil
	})
	if build.DEBUG && err != nil {
		panic(err)
	}
	for _, dscod := range dscods {
		pb.DelayedTurtleDexcoinOutputDiffs = append(pb.DelayedTurtleDexcoinOutputDiffs, dscod)
		commitDelayedTurtleDexcoinOutputDiff(tx, dscod, modules.DiffApply)
	}
	for _, fcd := range fcds {
		pb.FileContractDiffs = append(pb.FileContractDiffs, fcd)
		commitFileContractDiff(tx, fcd, modules.DiffApply)
	}
	err = tx.DeleteBucket(fceBucketID)
	if build.DEBUG && err != nil {
		panic(err)
	}
}

// applyMaintenance applies block-level alterations to the consensus set.
// Maintenance is applied after all of the transactions for the block have been
// applied.
func applyMaintenance(tx *bolt.Tx, pb *processedBlock) {
	applyMinerPayouts(tx, pb)
	applyFoundationSubsidy(tx, pb)
	applyMaturedTurtleDexcoinOutputs(tx, pb)
	applyFileContractMaintenance(tx, pb)
}