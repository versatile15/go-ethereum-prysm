package sync

import (
	"context"
	"fmt"

	libp2pcore "github.com/libp2p/go-libp2p/core"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/pkg/errors"
	"github.com/prysmaticlabs/prysm/v5/beacon-chain/execution"
	"github.com/prysmaticlabs/prysm/v5/beacon-chain/p2p/types"
	"github.com/prysmaticlabs/prysm/v5/beacon-chain/sync/verify"
	"github.com/prysmaticlabs/prysm/v5/beacon-chain/verification"
	fieldparams "github.com/prysmaticlabs/prysm/v5/config/fieldparams"
	"github.com/prysmaticlabs/prysm/v5/config/params"
	"github.com/prysmaticlabs/prysm/v5/consensus-types/blocks"
	"github.com/prysmaticlabs/prysm/v5/consensus-types/interfaces"
	eth "github.com/prysmaticlabs/prysm/v5/proto/prysm/v1alpha1"
	"github.com/prysmaticlabs/prysm/v5/runtime/version"
	"github.com/prysmaticlabs/prysm/v5/time/slots"
)

// sendRecentBeaconBlocksRequest sends a recent beacon blocks request to a peer to get
// those corresponding blocks from that peer.
func (s *Service) sendRecentBeaconBlocksRequest(ctx context.Context, requests *types.BeaconBlockByRootsReq, id peer.ID) error {
	ctx, cancel := context.WithTimeout(ctx, respTimeout)
	defer cancel()

	requestedRoots := make(map[[32]byte]struct{})
	for _, root := range *requests {
		requestedRoots[root] = struct{}{}
	}

	blks, err := SendBeaconBlocksByRootRequest(ctx, s.cfg.clock, s.cfg.p2p, id, requests, func(blk interfaces.ReadOnlySignedBeaconBlock) error {
		blkRoot, err := blk.Block().HashTreeRoot()
		if err != nil {
			return err
		}
		if _, ok := requestedRoots[blkRoot]; !ok {
			return fmt.Errorf("received unexpected block with root %x", blkRoot)
		}
		s.pendingQueueLock.Lock()
		defer s.pendingQueueLock.Unlock()
		if err := s.insertBlockToPendingQueue(blk.Block().Slot(), blk, blkRoot); err != nil {
			return err
		}
		return nil
	})
	for _, blk := range blks {
		// Skip blocks before deneb because they have no blob.
		if blk.Version() < version.Deneb {
			continue
		}
		blkRoot, err := blk.Block().HashTreeRoot()
		if err != nil {
			return err
		}
		request, err := s.pendingBlobsRequestForBlock(blkRoot, blk)
		if err != nil {
			return err
		}
		if len(request) == 0 {
			continue
		}
		if err := s.sendAndSaveBlobSidecars(ctx, request, id, blk); err != nil {
			return err
		}
	}
	return err
}

// beaconBlocksRootRPCHandler looks up the request blocks from the database from the given block roots.
func (s *Service) beaconBlocksRootRPCHandler(ctx context.Context, msg interface{}, stream libp2pcore.Stream) error {
	ctx, cancel := context.WithTimeout(ctx, ttfbTimeout)
	defer cancel()
	SetRPCStreamDeadlines(stream)
	log := log.WithField("handler", "beacon_blocks_by_root")

	rawMsg, ok := msg.(*types.BeaconBlockByRootsReq)
	if !ok {
		return errors.New("message is not type BeaconBlockByRootsReq")
	}
	blockRoots := *rawMsg
	if err := s.rateLimiter.validateRequest(stream, uint64(len(blockRoots))); err != nil {
		return err
	}
	if len(blockRoots) == 0 {
		// Add to rate limiter in the event no
		// roots are requested.
		s.rateLimiter.add(stream, 1)
		s.writeErrorResponseToStream(responseCodeInvalidRequest, "no block roots provided in request", stream)
		return errors.New("no block roots provided")
	}

	currentEpoch := slots.ToEpoch(s.cfg.clock.CurrentSlot())
	if uint64(len(blockRoots)) > params.MaxRequestBlock(currentEpoch) {
		s.cfg.p2p.Peers().Scorers().BadResponsesScorer().Increment(stream.Conn().RemotePeer())
		s.writeErrorResponseToStream(responseCodeInvalidRequest, "requested more than the max block limit", stream)
		return errors.New("requested more than the max block limit")
	}
	s.rateLimiter.add(stream, int64(len(blockRoots)))

	for _, root := range blockRoots {
		blk, err := s.cfg.beaconDB.Block(ctx, root)
		if err != nil {
			log.WithError(err).Debug("Could not fetch block")
			s.writeErrorResponseToStream(responseCodeServerError, types.ErrGeneric.Error(), stream)
			return err
		}
		if err := blocks.BeaconBlockIsNil(blk); err != nil {
			continue
		}

		if blk.Block().IsBlinded() {
			blk, err = s.cfg.executionReconstructor.ReconstructFullBlock(ctx, blk)
			if err != nil {
				if errors.Is(err, execution.ErrEmptyBlockHash) {
					log.WithError(err).Warn("Could not reconstruct block from header with syncing execution client. Waiting to complete syncing")
				} else {
					log.WithError(err).Error("Could not get reconstruct full block from blinded body")
				}
				s.writeErrorResponseToStream(responseCodeServerError, types.ErrGeneric.Error(), stream)
				return err
			}
		}

		if err := s.chunkBlockWriter(stream, blk); err != nil {
			return err
		}
	}

	closeStream(stream, log)
	return nil
}

// sendAndSaveBlobSidecars sends the blob request and saves received sidecars.
func (s *Service) sendAndSaveBlobSidecars(ctx context.Context, request types.BlobSidecarsByRootReq, peerID peer.ID, block interfaces.ReadOnlySignedBeaconBlock) error {
	if len(request) == 0 {
		return nil
	}

	sidecars, err := SendBlobSidecarByRoot(ctx, s.cfg.clock, s.cfg.p2p, peerID, s.ctxMap, &request)
	if err != nil {
		return err
	}

	RoBlock, err := blocks.NewROBlock(block)
	if err != nil {
		return err
	}
	if len(sidecars) != len(request) {
		return fmt.Errorf("received %d blob sidecars, expected %d for RPC", len(sidecars), len(request))
	}
	bv := verification.NewBlobBatchVerifier(s.newBlobVerifier, verification.PendingQueueSidecarRequirements)
	for _, sidecar := range sidecars {
		if err := verify.BlobAlignsWithBlock(sidecar, RoBlock); err != nil {
			return err
		}
		log.WithFields(blobFields(sidecar)).Debug("Received blob sidecar RPC")
	}
	vscs, err := bv.VerifiedROBlobs(ctx, RoBlock, sidecars)
	if err != nil {
		return err
	}
	for i := range vscs {
		if err := s.cfg.blobStorage.Save(vscs[i]); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) pendingBlobsRequestForBlock(root [32]byte, b interfaces.ReadOnlySignedBeaconBlock) (types.BlobSidecarsByRootReq, error) {
	if b.Version() < version.Deneb {
		return nil, nil // Block before deneb has no blob.
	}
	cc, err := b.Block().Body().BlobKzgCommitments()
	if err != nil {
		return nil, err
	}
	if len(cc) == 0 {
		return nil, nil
	}
	return s.constructPendingBlobsRequest(root, len(cc))
}

// constructPendingBlobsRequest creates a request for BlobSidecars by root, considering blobs already in DB.
func (s *Service) constructPendingBlobsRequest(root [32]byte, commitments int) (types.BlobSidecarsByRootReq, error) {
	if commitments == 0 {
		return nil, nil
	}
	stored, err := s.cfg.blobStorage.Indices(root)
	if err != nil {
		return nil, err
	}

	return requestsForMissingIndices(stored, commitments, root), nil
}

// requestsForMissingIndices constructs a slice of BlobIdentifiers that are missing from
// local storage, based on a mapping that represents which indices are locally stored,
// and the highest expected index.
func requestsForMissingIndices(storedIndices [fieldparams.MaxBlobsPerBlock]bool, commitments int, root [32]byte) []*eth.BlobIdentifier {
	var ids []*eth.BlobIdentifier
	for i := uint64(0); i < uint64(commitments); i++ {
		if !storedIndices[i] {
			ids = append(ids, &eth.BlobIdentifier{Index: i, BlockRoot: root[:]})
		}
	}
	return ids
}
