package beacon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/prysmaticlabs/prysm/v5/api"
	"github.com/prysmaticlabs/prysm/v5/api/server"
	"github.com/prysmaticlabs/prysm/v5/api/server/structs"
	"github.com/prysmaticlabs/prysm/v5/beacon-chain/core/blocks"
	"github.com/prysmaticlabs/prysm/v5/beacon-chain/core/feed"
	"github.com/prysmaticlabs/prysm/v5/beacon-chain/core/feed/operation"
	corehelpers "github.com/prysmaticlabs/prysm/v5/beacon-chain/core/helpers"
	"github.com/prysmaticlabs/prysm/v5/beacon-chain/core/transition"
	"github.com/prysmaticlabs/prysm/v5/beacon-chain/rpc/core"
	"github.com/prysmaticlabs/prysm/v5/beacon-chain/rpc/eth/shared"
	"github.com/prysmaticlabs/prysm/v5/config/features"
	consensus_types "github.com/prysmaticlabs/prysm/v5/consensus-types"
	"github.com/prysmaticlabs/prysm/v5/consensus-types/primitives"
	"github.com/prysmaticlabs/prysm/v5/crypto/bls"
	"github.com/prysmaticlabs/prysm/v5/monitoring/tracing/trace"
	"github.com/prysmaticlabs/prysm/v5/network/httputil"
	eth "github.com/prysmaticlabs/prysm/v5/proto/prysm/v1alpha1"
	"github.com/prysmaticlabs/prysm/v5/runtime/version"
	"github.com/prysmaticlabs/prysm/v5/time/slots"
)

const broadcastBLSChangesRateLimit = 128

// ListAttestations retrieves attestations known by the node but
// not necessarily incorporated into any block. Allows filtering by committee index or slot.
func (s *Server) ListAttestations(w http.ResponseWriter, r *http.Request) {
	_, span := trace.StartSpan(r.Context(), "beacon.ListAttestations")
	defer span.End()

	rawSlot, slot, ok := shared.UintFromQuery(w, r, "slot", false)
	if !ok {
		return
	}
	rawCommitteeIndex, committeeIndex, ok := shared.UintFromQuery(w, r, "committee_index", false)
	if !ok {
		return
	}

	attestations := s.AttestationsPool.AggregatedAttestations()
	unaggAtts, err := s.AttestationsPool.UnaggregatedAttestations()
	if err != nil {
		httputil.HandleError(w, "Could not get unaggregated attestations: "+err.Error(), http.StatusInternalServerError)
		return
	}
	attestations = append(attestations, unaggAtts...)

	filteredAtts := make([]*structs.Attestation, 0, len(attestations))
	for _, a := range attestations {
		var includeAttestation bool
		att, ok := a.(*eth.Attestation)
		if !ok {
			httputil.HandleError(w, fmt.Sprintf("Unable to convert attestation of type %T", a), http.StatusInternalServerError)
			return
		}

		includeAttestation = shouldIncludeAttestation(att.GetData(), rawSlot, slot, rawCommitteeIndex, committeeIndex)
		if includeAttestation {
			attStruct := structs.AttFromConsensus(att)
			filteredAtts = append(filteredAtts, attStruct)
		}
	}

	attsData, err := json.Marshal(filteredAtts)
	if err != nil {
		httputil.HandleError(w, "Could not marshal attestations: "+err.Error(), http.StatusInternalServerError)
		return
	}

	httputil.WriteJson(w, &structs.ListAttestationsResponse{
		Data: attsData,
	})
}

// ListAttestationsV2 retrieves attestations known by the node but
// not necessarily incorporated into any block. Allows filtering by committee index or slot.
func (s *Server) ListAttestationsV2(w http.ResponseWriter, r *http.Request) {
	ctx, span := trace.StartSpan(r.Context(), "beacon.ListAttestationsV2")
	defer span.End()

	rawSlot, slot, ok := shared.UintFromQuery(w, r, "slot", false)
	if !ok {
		return
	}
	rawCommitteeIndex, committeeIndex, ok := shared.UintFromQuery(w, r, "committee_index", false)
	if !ok {
		return
	}

	headState, err := s.ChainInfoFetcher.HeadStateReadOnly(ctx)
	if err != nil {
		httputil.HandleError(w, "Could not get head state: "+err.Error(), http.StatusInternalServerError)
		return
	}

	attestations := s.AttestationsPool.AggregatedAttestations()
	unaggAtts, err := s.AttestationsPool.UnaggregatedAttestations()
	if err != nil {
		httputil.HandleError(w, "Could not get unaggregated attestations: "+err.Error(), http.StatusInternalServerError)
		return
	}
	attestations = append(attestations, unaggAtts...)

	filteredAtts := make([]interface{}, 0, len(attestations))
	for _, att := range attestations {
		var includeAttestation bool
		if headState.Version() >= version.Electra {
			attElectra, ok := att.(*eth.AttestationElectra)
			if !ok {
				httputil.HandleError(w, fmt.Sprintf("Unable to convert attestation of type %T", att), http.StatusInternalServerError)
				return
			}

			includeAttestation = shouldIncludeAttestation(attElectra.GetData(), rawSlot, slot, rawCommitteeIndex, committeeIndex)
			if includeAttestation {
				attStruct := structs.AttElectraFromConsensus(attElectra)
				filteredAtts = append(filteredAtts, attStruct)
			}
		} else {
			attOld, ok := att.(*eth.Attestation)
			if !ok {
				httputil.HandleError(w, fmt.Sprintf("Unable to convert attestation of type %T", att), http.StatusInternalServerError)
				return
			}

			includeAttestation = shouldIncludeAttestation(attOld.GetData(), rawSlot, slot, rawCommitteeIndex, committeeIndex)
			if includeAttestation {
				attStruct := structs.AttFromConsensus(attOld)
				filteredAtts = append(filteredAtts, attStruct)
			}
		}
	}

	attsData, err := json.Marshal(filteredAtts)
	if err != nil {
		httputil.HandleError(w, "Could not marshal attestations: "+err.Error(), http.StatusInternalServerError)
		return
	}

	httputil.WriteJson(w, &structs.ListAttestationsResponse{
		Version: version.String(headState.Version()),
		Data:    attsData,
	})
}

// Helper function to determine if an attestation should be included
func shouldIncludeAttestation(
	data *eth.AttestationData,
	rawSlot string,
	slot uint64,
	rawCommitteeIndex string,
	committeeIndex uint64,
) bool {
	committeeIndexMatch := true
	slotMatch := true
	if rawCommitteeIndex != "" && data.CommitteeIndex != primitives.CommitteeIndex(committeeIndex) {
		committeeIndexMatch = false
	}
	if rawSlot != "" && data.Slot != primitives.Slot(slot) {
		slotMatch = false
	}
	return committeeIndexMatch && slotMatch
}

// SubmitAttestations submits an attestation object to node. If the attestation passes all validation
// constraints, node MUST publish the attestation on an appropriate subnet.
func (s *Server) SubmitAttestations(w http.ResponseWriter, r *http.Request) {
	ctx, span := trace.StartSpan(r.Context(), "beacon.SubmitAttestations")
	defer span.End()

	var req structs.SubmitAttestationsRequest
	err := json.NewDecoder(r.Body).Decode(&req.Data)
	switch {
	case errors.Is(err, io.EOF):
		httputil.HandleError(w, "No data submitted", http.StatusBadRequest)
		return
	case err != nil:
		httputil.HandleError(w, "Could not decode request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	attFailures, failedBroadcasts, err := s.handleAttestations(ctx, req.Data)
	if err != nil {
		httputil.HandleError(w, err.Error(), http.StatusBadRequest)
		return
	}

	if len(failedBroadcasts) > 0 {
		httputil.HandleError(
			w,
			fmt.Sprintf("Attestations at index %s could not be broadcasted", strings.Join(failedBroadcasts, ", ")),
			http.StatusInternalServerError,
		)
		return
	}

	if len(attFailures) > 0 {
		failuresErr := &server.IndexedVerificationFailureError{
			Code:     http.StatusBadRequest,
			Message:  "One or more attestations failed validation",
			Failures: attFailures,
		}
		httputil.WriteError(w, failuresErr)
	}
}

// SubmitAttestationsV2 submits an attestation object to node. If the attestation passes all validation
// constraints, node MUST publish the attestation on an appropriate subnet.
func (s *Server) SubmitAttestationsV2(w http.ResponseWriter, r *http.Request) {
	ctx, span := trace.StartSpan(r.Context(), "beacon.SubmitAttestationsV2")
	defer span.End()

	versionHeader := r.Header.Get(api.VersionHeader)
	if versionHeader == "" {
		httputil.HandleError(w, api.VersionHeader+" header is required", http.StatusBadRequest)
		return
	}
	v, err := version.FromString(versionHeader)
	if err != nil {
		httputil.HandleError(w, "Invalid version: "+err.Error(), http.StatusBadRequest)
		return
	}

	var req structs.SubmitAttestationsRequest
	err = json.NewDecoder(r.Body).Decode(&req.Data)
	switch {
	case errors.Is(err, io.EOF):
		httputil.HandleError(w, "No data submitted", http.StatusBadRequest)
		return
	case err != nil:
		httputil.HandleError(w, "Could not decode request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	var attFailures []*server.IndexedVerificationFailure
	var failedBroadcasts []string

	if v >= version.Electra {
		attFailures, failedBroadcasts, err = s.handleAttestationsElectra(ctx, req.Data)
	} else {
		attFailures, failedBroadcasts, err = s.handleAttestations(ctx, req.Data)
	}
	if err != nil {
		httputil.HandleError(w, fmt.Sprintf("Failed to handle attestations: %v", err), http.StatusBadRequest)
		return
	}

	if len(failedBroadcasts) > 0 {
		httputil.HandleError(
			w,
			fmt.Sprintf("Attestations at index %s could not be broadcasted", strings.Join(failedBroadcasts, ", ")),
			http.StatusInternalServerError,
		)
		return
	}

	if len(attFailures) > 0 {
		failuresErr := &server.IndexedVerificationFailureError{
			Code:     http.StatusBadRequest,
			Message:  "One or more attestations failed validation",
			Failures: attFailures,
		}
		httputil.WriteError(w, failuresErr)
	}
}

func (s *Server) handleAttestationsElectra(ctx context.Context, data json.RawMessage) (attFailures []*server.IndexedVerificationFailure, failedBroadcasts []string, err error) {
	var sourceAttestations []*structs.AttestationElectra

	if err = json.Unmarshal(data, &sourceAttestations); err != nil {
		return nil, nil, errors.Wrap(err, "failed to unmarshal attestation")
	}

	if len(sourceAttestations) == 0 {
		return nil, nil, errors.New("no data submitted")
	}

	var validAttestations []*eth.AttestationElectra
	for i, sourceAtt := range sourceAttestations {
		att, err := sourceAtt.ToConsensus()
		if err != nil {
			attFailures = append(attFailures, &server.IndexedVerificationFailure{
				Index:   i,
				Message: "Could not convert request attestation to consensus attestation: " + err.Error(),
			})
			continue
		}
		if _, err = bls.SignatureFromBytes(att.Signature); err != nil {
			attFailures = append(attFailures, &server.IndexedVerificationFailure{
				Index:   i,
				Message: "Incorrect attestation signature: " + err.Error(),
			})
			continue
		}
		validAttestations = append(validAttestations, att)
	}

	for i, att := range validAttestations {
		// Broadcast the unaggregated attestation on a feed to notify other services in the beacon node
		// of a received unaggregated attestation.
		// Note we can't send for aggregated att because we don't have selection proof.
		if !corehelpers.IsAggregated(att) {
			s.OperationNotifier.OperationFeed().Send(&feed.Event{
				Type: operation.UnaggregatedAttReceived,
				Data: &operation.UnAggregatedAttReceivedData{
					Attestation: att,
				},
			})
		}

		wantedEpoch := slots.ToEpoch(att.Data.Slot)
		vals, err := s.HeadFetcher.HeadValidatorsIndices(ctx, wantedEpoch)
		if err != nil {
			failedBroadcasts = append(failedBroadcasts, strconv.Itoa(i))
			continue
		}
		committeeIndex, err := att.GetCommitteeIndex()
		if err != nil {
			return nil, nil, errors.Wrap(err, "failed to retrieve attestation committee index")
		}
		subnet := corehelpers.ComputeSubnetFromCommitteeAndSlot(uint64(len(vals)), committeeIndex, att.Data.Slot)
		if err = s.Broadcaster.BroadcastAttestation(ctx, subnet, att); err != nil {
			log.WithError(err).Errorf("could not broadcast attestation at index %d", i)
			failedBroadcasts = append(failedBroadcasts, strconv.Itoa(i))
			continue
		}

		if corehelpers.IsAggregated(att) {
			if err = s.AttestationsPool.SaveAggregatedAttestation(att); err != nil {
				log.WithError(err).Error("could not save aggregated attestation")
			}
		} else {
			if err = s.AttestationsPool.SaveUnaggregatedAttestation(att); err != nil {
				log.WithError(err).Error("could not save unaggregated attestation")
			}
		}
	}

	return attFailures, failedBroadcasts, nil
}

func (s *Server) handleAttestations(ctx context.Context, data json.RawMessage) (attFailures []*server.IndexedVerificationFailure, failedBroadcasts []string, err error) {
	var sourceAttestations []*structs.Attestation

	if err = json.Unmarshal(data, &sourceAttestations); err != nil {
		return nil, nil, errors.Wrap(err, "failed to unmarshal attestation")
	}

	if len(sourceAttestations) == 0 {
		return nil, nil, errors.New("no data submitted")
	}

	var validAttestations []*eth.Attestation
	for i, sourceAtt := range sourceAttestations {
		att, err := sourceAtt.ToConsensus()
		if err != nil {
			attFailures = append(attFailures, &server.IndexedVerificationFailure{
				Index:   i,
				Message: "Could not convert request attestation to consensus attestation: " + err.Error(),
			})
			continue
		}
		if _, err = bls.SignatureFromBytes(att.Signature); err != nil {
			attFailures = append(attFailures, &server.IndexedVerificationFailure{
				Index:   i,
				Message: "Incorrect attestation signature: " + err.Error(),
			})
			continue
		}
		validAttestations = append(validAttestations, att)
	}

	for i, att := range validAttestations {
		// Broadcast the unaggregated attestation on a feed to notify other services in the beacon node
		// of a received unaggregated attestation.
		// Note we can't send for aggregated att because we don't have selection proof.
		if !corehelpers.IsAggregated(att) {
			s.OperationNotifier.OperationFeed().Send(&feed.Event{
				Type: operation.UnaggregatedAttReceived,
				Data: &operation.UnAggregatedAttReceivedData{
					Attestation: att,
				},
			})
		}

		wantedEpoch := slots.ToEpoch(att.Data.Slot)
		vals, err := s.HeadFetcher.HeadValidatorsIndices(ctx, wantedEpoch)
		if err != nil {
			failedBroadcasts = append(failedBroadcasts, strconv.Itoa(i))
			continue
		}

		subnet := corehelpers.ComputeSubnetFromCommitteeAndSlot(uint64(len(vals)), att.Data.CommitteeIndex, att.Data.Slot)
		if err = s.Broadcaster.BroadcastAttestation(ctx, subnet, att); err != nil {
			log.WithError(err).Errorf("could not broadcast attestation at index %d", i)
			failedBroadcasts = append(failedBroadcasts, strconv.Itoa(i))
			continue
		}

		if corehelpers.IsAggregated(att) {
			if err = s.AttestationsPool.SaveAggregatedAttestation(att); err != nil {
				log.WithError(err).Error("could not save aggregated attestation")
			}
		} else {
			if err = s.AttestationsPool.SaveUnaggregatedAttestation(att); err != nil {
				log.WithError(err).Error("could not save unaggregated attestation")
			}
		}
	}

	return attFailures, failedBroadcasts, nil
}

// ListVoluntaryExits retrieves voluntary exits known by the node but
// not necessarily incorporated into any block.
func (s *Server) ListVoluntaryExits(w http.ResponseWriter, r *http.Request) {
	_, span := trace.StartSpan(r.Context(), "beacon.ListVoluntaryExits")
	defer span.End()

	sourceExits, err := s.VoluntaryExitsPool.PendingExits()
	if err != nil {
		httputil.HandleError(w, "Could not get exits from the pool: "+err.Error(), http.StatusInternalServerError)
		return
	}
	exits := make([]*structs.SignedVoluntaryExit, len(sourceExits))
	for i, e := range sourceExits {
		exits[i] = structs.SignedExitFromConsensus(e)
	}

	httputil.WriteJson(w, &structs.ListVoluntaryExitsResponse{Data: exits})
}

// SubmitVoluntaryExit submits a SignedVoluntaryExit object to node's pool
// and if passes validation node MUST broadcast it to network.
func (s *Server) SubmitVoluntaryExit(w http.ResponseWriter, r *http.Request) {
	ctx, span := trace.StartSpan(r.Context(), "beacon.SubmitVoluntaryExit")
	defer span.End()

	var req structs.SignedVoluntaryExit
	err := json.NewDecoder(r.Body).Decode(&req)
	switch {
	case errors.Is(err, io.EOF):
		httputil.HandleError(w, "No data submitted", http.StatusBadRequest)
		return
	case err != nil:
		httputil.HandleError(w, "Could not decode request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	exit, err := req.ToConsensus()
	if err != nil {
		httputil.HandleError(w, "Could not convert request exit to consensus exit: "+err.Error(), http.StatusBadRequest)
		return
	}

	headState, err := s.ChainInfoFetcher.HeadState(ctx)
	if err != nil {
		httputil.HandleError(w, "Could not get head state: "+err.Error(), http.StatusInternalServerError)
		return
	}
	epochStart, err := slots.EpochStart(exit.Exit.Epoch)
	if err != nil {
		httputil.HandleError(w, "Could not get epoch start: "+err.Error(), http.StatusInternalServerError)
		return
	}
	headState, err = transition.ProcessSlotsIfPossible(ctx, headState, epochStart)
	if err != nil {
		httputil.HandleError(w, "Could not process slots: "+err.Error(), http.StatusInternalServerError)
		return
	}
	val, err := headState.ValidatorAtIndexReadOnly(exit.Exit.ValidatorIndex)
	if err != nil {
		if errors.Is(err, consensus_types.ErrOutOfBounds) {
			httputil.HandleError(w, "Could not get validator: "+err.Error(), http.StatusBadRequest)
			return
		}
		httputil.HandleError(w, "Could not get validator: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err = blocks.VerifyExitAndSignature(val, headState, exit); err != nil {
		httputil.HandleError(w, "Invalid exit: "+err.Error(), http.StatusBadRequest)
		return
	}

	s.VoluntaryExitsPool.InsertVoluntaryExit(exit)
	if err = s.Broadcaster.Broadcast(ctx, exit); err != nil {
		httputil.HandleError(w, "Could not broadcast exit: "+err.Error(), http.StatusInternalServerError)
		return
	}
}

// SubmitSyncCommitteeSignatures submits sync committee signature objects to the node.
func (s *Server) SubmitSyncCommitteeSignatures(w http.ResponseWriter, r *http.Request) {
	ctx, span := trace.StartSpan(r.Context(), "beacon.SubmitPoolSyncCommitteeSignatures")
	defer span.End()

	var req structs.SubmitSyncCommitteeSignaturesRequest
	err := json.NewDecoder(r.Body).Decode(&req.Data)
	switch {
	case errors.Is(err, io.EOF):
		httputil.HandleError(w, "No data submitted", http.StatusBadRequest)
		return
	case err != nil:
		httputil.HandleError(w, "Could not decode request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.Data) == 0 {
		httputil.HandleError(w, "No data submitted", http.StatusBadRequest)
		return
	}

	var validMessages []*eth.SyncCommitteeMessage
	var msgFailures []*server.IndexedVerificationFailure
	for i, sourceMsg := range req.Data {
		msg, err := sourceMsg.ToConsensus()
		if err != nil {
			msgFailures = append(msgFailures, &server.IndexedVerificationFailure{
				Index:   i,
				Message: "Could not convert request message to consensus message: " + err.Error(),
			})
			continue
		}
		validMessages = append(validMessages, msg)
	}

	for _, msg := range validMessages {
		if rpcerr := s.CoreService.SubmitSyncMessage(ctx, msg); rpcerr != nil {
			httputil.HandleError(w, "Could not submit message: "+rpcerr.Err.Error(), core.ErrorReasonToHTTP(rpcerr.Reason))
			return
		}
	}

	if len(msgFailures) > 0 {
		failuresErr := &server.IndexedVerificationFailureError{
			Code:     http.StatusBadRequest,
			Message:  "One or more messages failed validation",
			Failures: msgFailures,
		}
		httputil.WriteError(w, failuresErr)
	}
}

// SubmitBLSToExecutionChanges submits said object to the node's pool
// if it passes validation the node must broadcast it to the network.
func (s *Server) SubmitBLSToExecutionChanges(w http.ResponseWriter, r *http.Request) {
	ctx, span := trace.StartSpan(r.Context(), "beacon.SubmitBLSToExecutionChanges")
	defer span.End()
	st, err := s.ChainInfoFetcher.HeadStateReadOnly(ctx)
	if err != nil {
		httputil.HandleError(w, fmt.Sprintf("Could not get head state: %v", err), http.StatusInternalServerError)
		return
	}
	var failures []*server.IndexedVerificationFailure
	var toBroadcast []*eth.SignedBLSToExecutionChange

	var req []*structs.SignedBLSToExecutionChange
	err = json.NewDecoder(r.Body).Decode(&req)
	switch {
	case errors.Is(err, io.EOF):
		httputil.HandleError(w, "No data submitted", http.StatusBadRequest)
		return
	case err != nil:
		httputil.HandleError(w, "Could not decode request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(req) == 0 {
		httputil.HandleError(w, "No data submitted", http.StatusBadRequest)
		return
	}

	for i, change := range req {
		sbls, err := change.ToConsensus()
		if err != nil {
			failures = append(failures, &server.IndexedVerificationFailure{
				Index:   i,
				Message: "Unable to decode SignedBLSToExecutionChange: " + err.Error(),
			})
			continue
		}
		_, err = blocks.ValidateBLSToExecutionChange(st, sbls)
		if err != nil {
			failures = append(failures, &server.IndexedVerificationFailure{
				Index:   i,
				Message: "Could not validate SignedBLSToExecutionChange: " + err.Error(),
			})
			continue
		}
		if err := blocks.VerifyBLSChangeSignature(st, sbls); err != nil {
			failures = append(failures, &server.IndexedVerificationFailure{
				Index:   i,
				Message: "Could not validate signature: " + err.Error(),
			})
			continue
		}
		s.OperationNotifier.OperationFeed().Send(&feed.Event{
			Type: operation.BLSToExecutionChangeReceived,
			Data: &operation.BLSToExecutionChangeReceivedData{
				Change: sbls,
			},
		})
		s.BLSChangesPool.InsertBLSToExecChange(sbls)
		if st.Version() >= version.Capella {
			toBroadcast = append(toBroadcast, sbls)
		}
	}
	go s.broadcastBLSChanges(ctx, toBroadcast)
	if len(failures) > 0 {
		failuresErr := &server.IndexedVerificationFailureError{
			Code:     http.StatusBadRequest,
			Message:  "One or more BLSToExecutionChange failed validation",
			Failures: failures,
		}
		httputil.WriteError(w, failuresErr)
	}
}

// broadcastBLSBatch broadcasts the first `broadcastBLSChangesRateLimit` messages from the slice pointed to by ptr.
// It validates the messages again because they could have been invalidated by being included in blocks since the last validation.
// It removes the messages from the slice and modifies it in place.
func (s *Server) broadcastBLSBatch(ctx context.Context, ptr *[]*eth.SignedBLSToExecutionChange) {
	limit := broadcastBLSChangesRateLimit
	if len(*ptr) < broadcastBLSChangesRateLimit {
		limit = len(*ptr)
	}
	st, err := s.ChainInfoFetcher.HeadStateReadOnly(ctx)
	if err != nil {
		log.WithError(err).Error("could not get head state")
		return
	}
	for _, ch := range (*ptr)[:limit] {
		if ch != nil {
			_, err := blocks.ValidateBLSToExecutionChange(st, ch)
			if err != nil {
				log.WithError(err).Error("could not validate BLS to execution change")
				continue
			}
			if err := s.Broadcaster.Broadcast(ctx, ch); err != nil {
				log.WithError(err).Error("could not broadcast BLS to execution changes.")
			}
		}
	}
	*ptr = (*ptr)[limit:]
}

func (s *Server) broadcastBLSChanges(ctx context.Context, changes []*eth.SignedBLSToExecutionChange) {
	s.broadcastBLSBatch(ctx, &changes)
	if len(changes) == 0 {
		return
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.broadcastBLSBatch(ctx, &changes)
			if len(changes) == 0 {
				return
			}
		}
	}
}

// ListBLSToExecutionChanges retrieves BLS to execution changes known by the node but not necessarily incorporated into any block
func (s *Server) ListBLSToExecutionChanges(w http.ResponseWriter, r *http.Request) {
	_, span := trace.StartSpan(r.Context(), "beacon.ListBLSToExecutionChanges")
	defer span.End()

	sourceChanges, err := s.BLSChangesPool.PendingBLSToExecChanges()
	if err != nil {
		httputil.HandleError(w, fmt.Sprintf("Could not get BLS to execution changes: %v", err), http.StatusInternalServerError)
		return
	}

	httputil.WriteJson(w, &structs.BLSToExecutionChangesPoolResponse{
		Data: structs.SignedBLSChangesFromConsensus(sourceChanges),
	})
}

// GetAttesterSlashings retrieves attester slashings known by the node but
// not necessarily incorporated into any block.
func (s *Server) GetAttesterSlashings(w http.ResponseWriter, r *http.Request) {
	ctx, span := trace.StartSpan(r.Context(), "beacon.GetAttesterSlashings")
	defer span.End()

	headState, err := s.ChainInfoFetcher.HeadStateReadOnly(ctx)
	if err != nil {
		httputil.HandleError(w, "Could not get head state: "+err.Error(), http.StatusInternalServerError)
		return
	}
	sourceSlashings := s.SlashingsPool.PendingAttesterSlashings(ctx, headState, true /* return unlimited slashings */)
	slashings := make([]*structs.AttesterSlashing, len(sourceSlashings))
	for i, slashing := range sourceSlashings {
		as, ok := slashing.(*eth.AttesterSlashing)
		if !ok {
			httputil.HandleError(w, fmt.Sprintf("Unable to convert slashing of type %T", slashing), http.StatusInternalServerError)
			return
		}
		slashings[i] = structs.AttesterSlashingFromConsensus(as)
	}
	attBytes, err := json.Marshal(slashings)
	if err != nil {
		httputil.HandleError(w, fmt.Sprintf("Failed to marshal slashings: %v", err), http.StatusInternalServerError)
		return
	}
	httputil.WriteJson(w, &structs.GetAttesterSlashingsResponse{Data: attBytes})
}

// GetAttesterSlashingsV2 retrieves attester slashings known by the node but
// not necessarily incorporated into any block, supporting both AttesterSlashing and AttesterSlashingElectra.
func (s *Server) GetAttesterSlashingsV2(w http.ResponseWriter, r *http.Request) {
	ctx, span := trace.StartSpan(r.Context(), "beacon.GetAttesterSlashingsV2")
	defer span.End()

	headState, err := s.ChainInfoFetcher.HeadStateReadOnly(ctx)
	if err != nil {
		httputil.HandleError(w, "Could not get head state: "+err.Error(), http.StatusInternalServerError)
		return
	}

	var attStructs []interface{}
	sourceSlashings := s.SlashingsPool.PendingAttesterSlashings(ctx, headState, true /* return unlimited slashings */)

	for _, slashing := range sourceSlashings {
		var attStruct interface{}
		if headState.Version() >= version.Electra {
			a, ok := slashing.(*eth.AttesterSlashingElectra)
			if !ok {
				httputil.HandleError(w, fmt.Sprintf("Unable to convert slashing of type %T to an Electra slashing", slashing), http.StatusInternalServerError)
				return
			}
			attStruct = structs.AttesterSlashingElectraFromConsensus(a)
		} else {
			a, ok := slashing.(*eth.AttesterSlashing)
			if !ok {
				httputil.HandleError(w, fmt.Sprintf("Unable to convert slashing of type %T to a Phase0 slashing", slashing), http.StatusInternalServerError)
				return
			}
			attStruct = structs.AttesterSlashingFromConsensus(a)
		}
		attStructs = append(attStructs, attStruct)
	}

	attBytes, err := json.Marshal(attStructs)
	if err != nil {
		httputil.HandleError(w, fmt.Sprintf("Failed to marshal slashing: %v", err), http.StatusInternalServerError)
		return
	}

	resp := &structs.GetAttesterSlashingsResponse{
		Version: version.String(headState.Version()),
		Data:    attBytes,
	}
	w.Header().Set(api.VersionHeader, version.String(headState.Version()))
	httputil.WriteJson(w, resp)
}

// SubmitAttesterSlashings submits an attester slashing object to node's pool and
// if passes validation node MUST broadcast it to network.
func (s *Server) SubmitAttesterSlashings(w http.ResponseWriter, r *http.Request) {
	ctx, span := trace.StartSpan(r.Context(), "beacon.SubmitAttesterSlashings")
	defer span.End()

	var req structs.AttesterSlashing
	err := json.NewDecoder(r.Body).Decode(&req)
	switch {
	case errors.Is(err, io.EOF):
		httputil.HandleError(w, "No data submitted", http.StatusBadRequest)
		return
	case err != nil:
		httputil.HandleError(w, "Could not decode request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	slashing, err := req.ToConsensus()
	if err != nil {
		httputil.HandleError(w, "Could not convert request slashing to consensus slashing: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.submitAttesterSlashing(w, ctx, slashing)
}

// SubmitAttesterSlashingsV2 submits an attester slashing object to node's pool and
// if passes validation node MUST broadcast it to network.
func (s *Server) SubmitAttesterSlashingsV2(w http.ResponseWriter, r *http.Request) {
	ctx, span := trace.StartSpan(r.Context(), "beacon.SubmitAttesterSlashingsV2")
	defer span.End()

	versionHeader := r.Header.Get(api.VersionHeader)
	if versionHeader == "" {
		httputil.HandleError(w, api.VersionHeader+" header is required", http.StatusBadRequest)
	}
	v, err := version.FromString(versionHeader)
	if err != nil {
		httputil.HandleError(w, "Invalid version: "+err.Error(), http.StatusBadRequest)
		return
	}

	if v >= version.Electra {
		var req structs.AttesterSlashingElectra
		err := json.NewDecoder(r.Body).Decode(&req)
		switch {
		case errors.Is(err, io.EOF):
			httputil.HandleError(w, "No data submitted", http.StatusBadRequest)
			return
		case err != nil:
			httputil.HandleError(w, "Could not decode request body: "+err.Error(), http.StatusBadRequest)
			return
		}

		slashing, err := req.ToConsensus()
		if err != nil {
			httputil.HandleError(w, "Could not convert request slashing to consensus slashing: "+err.Error(), http.StatusBadRequest)
			return
		}
		s.submitAttesterSlashing(w, ctx, slashing)
	} else {
		var req structs.AttesterSlashing
		err := json.NewDecoder(r.Body).Decode(&req)
		switch {
		case errors.Is(err, io.EOF):
			httputil.HandleError(w, "No data submitted", http.StatusBadRequest)
			return
		case err != nil:
			httputil.HandleError(w, "Could not decode request body: "+err.Error(), http.StatusBadRequest)
			return
		}

		slashing, err := req.ToConsensus()
		if err != nil {
			httputil.HandleError(w, "Could not convert request slashing to consensus slashing: "+err.Error(), http.StatusBadRequest)
			return
		}
		s.submitAttesterSlashing(w, ctx, slashing)
	}
}

func (s *Server) submitAttesterSlashing(
	w http.ResponseWriter,
	ctx context.Context,
	slashing eth.AttSlashing,
) {
	headState, err := s.ChainInfoFetcher.HeadState(ctx)
	if err != nil {
		httputil.HandleError(w, "Could not get head state: "+err.Error(), http.StatusInternalServerError)
		return
	}
	headState, err = transition.ProcessSlotsIfPossible(ctx, headState, slashing.FirstAttestation().GetData().Slot)
	if err != nil {
		httputil.HandleError(w, "Could not process slots: "+err.Error(), http.StatusInternalServerError)
		return
	}

	err = blocks.VerifyAttesterSlashing(ctx, headState, slashing)
	if err != nil {
		httputil.HandleError(w, "Invalid attester slashing: "+err.Error(), http.StatusBadRequest)
		return
	}
	err = s.SlashingsPool.InsertAttesterSlashing(ctx, headState, slashing)
	if err != nil {
		httputil.HandleError(w, "Could not insert attester slashing into pool: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// notify events
	s.OperationNotifier.OperationFeed().Send(&feed.Event{
		Type: operation.AttesterSlashingReceived,
		Data: &operation.AttesterSlashingReceivedData{
			AttesterSlashing: slashing,
		},
	})
	if !features.Get().DisableBroadcastSlashings {
		if err = s.Broadcaster.Broadcast(ctx, slashing); err != nil {
			httputil.HandleError(w, "Could not broadcast slashing object: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
}

// GetProposerSlashings retrieves proposer slashings known by the node
// but not necessarily incorporated into any block.
func (s *Server) GetProposerSlashings(w http.ResponseWriter, r *http.Request) {
	ctx, span := trace.StartSpan(r.Context(), "beacon.GetProposerSlashings")
	defer span.End()

	headState, err := s.ChainInfoFetcher.HeadStateReadOnly(ctx)
	if err != nil {
		httputil.HandleError(w, "Could not get head state: "+err.Error(), http.StatusInternalServerError)
		return
	}
	sourceSlashings := s.SlashingsPool.PendingProposerSlashings(ctx, headState, true /* return unlimited slashings */)
	slashings := structs.ProposerSlashingsFromConsensus(sourceSlashings)

	httputil.WriteJson(w, &structs.GetProposerSlashingsResponse{Data: slashings})
}

// SubmitProposerSlashing submits a proposer slashing object to node's pool and if
// passes validation node MUST broadcast it to network.
func (s *Server) SubmitProposerSlashing(w http.ResponseWriter, r *http.Request) {
	ctx, span := trace.StartSpan(r.Context(), "beacon.SubmitProposerSlashing")
	defer span.End()

	var req structs.ProposerSlashing
	err := json.NewDecoder(r.Body).Decode(&req)
	switch {
	case errors.Is(err, io.EOF):
		httputil.HandleError(w, "No data submitted", http.StatusBadRequest)
		return
	case err != nil:
		httputil.HandleError(w, "Could not decode request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	slashing, err := req.ToConsensus()
	if err != nil {
		httputil.HandleError(w, "Could not convert request slashing to consensus slashing: "+err.Error(), http.StatusBadRequest)
		return
	}
	headState, err := s.ChainInfoFetcher.HeadState(ctx)
	if err != nil {
		httputil.HandleError(w, "Could not get head state: "+err.Error(), http.StatusInternalServerError)
		return
	}
	headState, err = transition.ProcessSlotsIfPossible(ctx, headState, slashing.Header_1.Header.Slot)
	if err != nil {
		httputil.HandleError(w, "Could not process slots: "+err.Error(), http.StatusInternalServerError)
		return
	}
	err = blocks.VerifyProposerSlashing(headState, slashing)
	if err != nil {
		httputil.HandleError(w, "Invalid proposer slashing: "+err.Error(), http.StatusBadRequest)
		return
	}

	err = s.SlashingsPool.InsertProposerSlashing(ctx, headState, slashing)
	if err != nil {
		httputil.HandleError(w, "Could not insert proposer slashing into pool: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// notify events
	s.OperationNotifier.OperationFeed().Send(&feed.Event{
		Type: operation.ProposerSlashingReceived,
		Data: &operation.ProposerSlashingReceivedData{
			ProposerSlashing: slashing,
		},
	})

	if !features.Get().DisableBroadcastSlashings {
		if err = s.Broadcaster.Broadcast(ctx, slashing); err != nil {
			httputil.HandleError(w, "Could not broadcast slashing object: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
}
