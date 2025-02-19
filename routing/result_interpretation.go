package routing

import (
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// Instantiate variables to allow taking a reference from the failure reason.
var (
	reasonError            = channeldb.FailureReasonError
	reasonIncorrectDetails = channeldb.FailureReasonIncorrectPaymentDetails
)

// interpretedResult contains the result of the interpretation of a payment
// outcome.
type interpretedResult struct {
	// nodeFailure points to a node pubkey if all channels of that node are
	// responsible for the result.
	nodeFailure *route.Vertex

	// pairResults contains a map of node pairs that could be responsible
	// for the failure. The map values are the minimum amounts for which a
	// future penalty should be applied.
	pairResults map[DirectedNodePair]lnwire.MilliSatoshi

	// finalFailureReason is set to a non-nil value if it makes no more
	// sense to start another payment attempt. It will contain the reason
	// why.
	finalFailureReason *channeldb.FailureReason

	// policyFailure is set to a node pair if there is a policy failure on
	// that connection. This is used to control the second chance logic for
	// policy failures.
	policyFailure *DirectedNodePair
}

// interpretResult interprets a payment outcome and returns an object that
// contains information required to update mission control.
func interpretResult(rt *route.Route, failureSrcIdx *int,
	failure lnwire.FailureMessage) *interpretedResult {

	i := &interpretedResult{
		pairResults: make(map[DirectedNodePair]lnwire.MilliSatoshi),
	}

	i.processFail(rt, failureSrcIdx, failure)

	return i
}

// processFail processes a failed payment attempt.
func (i *interpretedResult) processFail(
	rt *route.Route, errSourceIdx *int,
	failure lnwire.FailureMessage) {

	if errSourceIdx == nil {
		i.processPaymentOutcomeUnknown(rt)
		return
	}

	switch *errSourceIdx {

	// We are the source of the failure.
	case 0:
		i.processPaymentOutcomeSelf(rt, failure)

	// A failure from the final hop was received.
	case len(rt.Hops):
		i.processPaymentOutcomeFinal(
			rt, failure,
		)

	// An intermediate hop failed. Interpret the outcome, update reputation
	// and try again.
	default:
		i.processPaymentOutcomeIntermediate(
			rt, *errSourceIdx, failure,
		)
	}
}

// processPaymentOutcomeSelf handles failures sent by ourselves.
func (i *interpretedResult) processPaymentOutcomeSelf(
	rt *route.Route, failure lnwire.FailureMessage) {

	switch failure.(type) {

	// We receive a malformed htlc failure from our peer. We trust ourselves
	// to send the correct htlc, so our peer must be at fault.
	case *lnwire.FailInvalidOnionVersion,
		*lnwire.FailInvalidOnionHmac,
		*lnwire.FailInvalidOnionKey:

		i.failNode(rt, 1)

		// If this was a payment to a direct peer, we can stop trying.
		if len(rt.Hops) == 1 {
			i.finalFailureReason = &reasonError
		}

	// Any other failure originating from ourselves should be temporary and
	// caused by changing conditions between path finding and execution of
	// the payment. We just retry and trust that the information locally
	// available in the link has been updated.
	default:
		log.Warnf("Routing failure for local channel %v occurred",
			rt.Hops[0].ChannelID)
	}
}

// processPaymentOutcomeFinal handles failures sent by the final hop.
func (i *interpretedResult) processPaymentOutcomeFinal(
	route *route.Route, failure lnwire.FailureMessage) {

	n := len(route.Hops)

	// If a failure from the final node is received, we will fail the
	// payment in almost all cases. Only when the penultimate node sends an
	// incorrect htlc, we want to retry via another route. Invalid onion
	// failures are not expected, because the final node wouldn't be able to
	// encrypt that failure.
	switch failure.(type) {

	// Expiry or amount of the HTLC doesn't match the onion, try another
	// route.
	case *lnwire.FailFinalIncorrectCltvExpiry,
		*lnwire.FailFinalIncorrectHtlcAmount:

		// We trust ourselves. If this is a direct payment, we penalize
		// the final node and fail the payment.
		if n == 1 {
			i.failNode(route, n)
			i.finalFailureReason = &reasonError

			return
		}

		// Otherwise penalize the last pair of the route and retry.
		// Either the final node is at fault, or it gets sent a bad htlc
		// from its predecessor.
		i.failPair(route, n-1)

	// We are using wrong payment hash or amount, fail the payment.
	case *lnwire.FailIncorrectPaymentAmount,
		*lnwire.FailIncorrectDetails:

		i.finalFailureReason = &reasonIncorrectDetails

	// The HTLC that was extended to the final hop expires too soon. Fail
	// the payment, because we may be using the wrong final cltv delta.
	case *lnwire.FailFinalExpiryTooSoon:
		// TODO(roasbeef): can happen to to race condition, try again
		// with recent block height

		// TODO(joostjager): can also happen because a node delayed
		// deliberately. What to penalize?
		i.finalFailureReason = &reasonIncorrectDetails

	default:
		// All other errors are considered terminal if coming from the
		// final hop. They indicate that something is wrong at the
		// recipient, so we do apply a penalty.
		i.failNode(route, n)
		i.finalFailureReason = &reasonError
	}
}

// processPaymentOutcomeIntermediate handles failures sent by an intermediate
// hop.
func (i *interpretedResult) processPaymentOutcomeIntermediate(
	route *route.Route, errorSourceIdx int,
	failure lnwire.FailureMessage) {

	reportOutgoing := func() {
		i.failPair(
			route, errorSourceIdx,
		)
	}

	reportOutgoingBalance := func() {
		i.failPairBalance(
			route, errorSourceIdx,
		)
	}

	reportIncoming := func() {
		// We trust ourselves. If the error comes from the first hop, we
		// can penalize the whole node. In that case there is no
		// uncertainty as to which node to blame.
		if errorSourceIdx == 1 {
			i.failNode(route, errorSourceIdx)
			return
		}

		// Otherwise report the incoming pair.
		i.failPair(
			route, errorSourceIdx-1,
		)
	}

	reportAll := func() {
		// We trust ourselves. If the error comes from the first hop, we
		// can penalize the whole node. In that case there is no
		// uncertainty as to which node to blame.
		if errorSourceIdx == 1 {
			i.failNode(route, errorSourceIdx)
			return
		}

		// Otherwise penalize all pairs up to the error source. This
		// includes our own outgoing connection.
		i.failPairRange(
			route, 0, errorSourceIdx-1,
		)
	}

	switch failure.(type) {

	// If a node reports onion payload corruption or an invalid version,
	// that node may be responsible, but it could also be that it is just
	// relaying a malformed htlc failure from it successor. By reporting the
	// outgoing channel set, we will surely hit the responsible node. At
	// this point, it is not possible that the node's predecessor corrupted
	// the onion blob. If the predecessor would have corrupted the payload,
	// the error source wouldn't have been able to encrypt this failure
	// message for us.
	case *lnwire.FailInvalidOnionVersion,
		*lnwire.FailInvalidOnionHmac,
		*lnwire.FailInvalidOnionKey:

		reportOutgoing()

	// If the next hop in the route wasn't known or offline, we'll only
	// penalize the channel set which we attempted to route over. This is
	// conservative, and it can handle faulty channels between nodes
	// properly. Additionally, this guards against routing nodes returning
	// errors in order to attempt to black list another node.
	case *lnwire.FailUnknownNextPeer:
		reportOutgoing()

	// If we get a permanent channel, we'll prune the channel set in both
	// directions and continue with the rest of the routes.
	case *lnwire.FailPermanentChannelFailure:
		reportOutgoing()

	// When an HTLC parameter is incorrect, the node sending the error may
	// be doing something wrong. But it could also be that its predecessor
	// is intentionally modifying the htlc parameters that we instructed it
	// via the hop payload. Therefore we penalize the incoming node pair. A
	// third cause of this error may be that we have an out of date channel
	// update. This is handled by the second chance logic up in mission
	// control.
	case *lnwire.FailAmountBelowMinimum,
		*lnwire.FailFeeInsufficient,
		*lnwire.FailIncorrectCltvExpiry,
		*lnwire.FailChannelDisabled:

		// Set the node pair for which a channel update may be out of
		// date. The second chance logic uses the policyFailure field.
		i.policyFailure = &DirectedNodePair{
			From: route.Hops[errorSourceIdx-1].PubKeyBytes,
			To:   route.Hops[errorSourceIdx].PubKeyBytes,
		}

		// We report incoming channel. If a second pair is granted in
		// mission control, this report is ignored.
		reportIncoming()

	// If the outgoing channel doesn't have enough capacity, we penalize.
	// But we penalize only in a single direction and only for amounts
	// greater than the attempted amount.
	case *lnwire.FailTemporaryChannelFailure:
		reportOutgoingBalance()

	// If FailExpiryTooSoon is received, there must have been some delay
	// along the path. We can't know which node is causing the delay, so we
	// penalize all of them up to the error source.
	//
	// Alternatively it could also be that we ourselves have fallen behind
	// somehow. We ignore that case for now.
	case *lnwire.FailExpiryTooSoon:
		reportAll()

	// In all other cases, we penalize the reporting node. These are all
	// failures that should not happen.
	default:
		i.failNode(route, errorSourceIdx)
	}
}

// processPaymentOutcomeUnknown processes a payment outcome for which no failure
// message or source is available.
func (i *interpretedResult) processPaymentOutcomeUnknown(route *route.Route) {
	n := len(route.Hops)

	// If this is a direct payment, the destination must be at fault.
	if n == 1 {
		i.failNode(route, n)
		i.finalFailureReason = &reasonError
		return
	}

	// Otherwise penalize all channels in the route to make sure the
	// responsible node is at least hit too. We even penalize the connection
	// to our own peer, because that peer could also be responsible.
	i.failPairRange(route, 0, n-1)
}

// failNode marks the node indicated by idx in the route as failed. This
// function intentionally panics when the self node is failed.
func (i *interpretedResult) failNode(rt *route.Route, idx int) {
	i.nodeFailure = &rt.Hops[idx-1].PubKeyBytes
}

// failPairRange marks the node pairs from node fromIdx to node toIdx as failed
// in both direction.
func (i *interpretedResult) failPairRange(
	rt *route.Route, fromIdx, toIdx int) {

	for idx := fromIdx; idx <= toIdx; idx++ {
		i.failPair(rt, idx)
	}
}

// failPair marks a pair as failed in both directions.
func (i *interpretedResult) failPair(
	rt *route.Route, idx int) {

	pair, _ := getPair(rt, idx)

	// Report pair in both directions without a minimum penalization amount.
	i.pairResults[pair] = 0
	i.pairResults[pair.Reverse()] = 0
}

// failPairBalance marks a pair as failed with a minimum penalization amount.
func (i *interpretedResult) failPairBalance(
	rt *route.Route, channelIdx int) {

	pair, amt := getPair(rt, channelIdx)

	i.pairResults[pair] = amt
}

// getPair returns a node pair from the route and the amount passed between that
// pair.
func getPair(rt *route.Route, channelIdx int) (DirectedNodePair,
	lnwire.MilliSatoshi) {

	nodeTo := rt.Hops[channelIdx].PubKeyBytes
	var (
		nodeFrom route.Vertex
		amt      lnwire.MilliSatoshi
	)

	if channelIdx == 0 {
		nodeFrom = rt.SourcePubKey
		amt = rt.TotalAmount
	} else {
		nodeFrom = rt.Hops[channelIdx-1].PubKeyBytes
		amt = rt.Hops[channelIdx-1].AmtToForward
	}

	pair := NewDirectedNodePair(nodeFrom, nodeTo)

	return pair, amt
}
