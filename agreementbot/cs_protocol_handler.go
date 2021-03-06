package agreementbot

import (
	"errors"
	"fmt"
	"github.com/boltdb/bolt"
	"github.com/golang/glog"
	"github.com/open-horizon/anax/abstractprotocol"
	"github.com/open-horizon/anax/citizenscientist"
	"github.com/open-horizon/anax/config"
	"github.com/open-horizon/anax/ethblockchain"
	"github.com/open-horizon/anax/events"
	"github.com/open-horizon/anax/exchange"
	"github.com/open-horizon/anax/metering"
	"github.com/open-horizon/anax/policy"
	"github.com/open-horizon/anax/worker"
	"math/rand"
	"sync"
	"time"
)

type BlockchainState struct {
	ready       bool                              // the blockchain is ready
	writable    bool                              // the blockchain is writable
	service     string                            // the network endpoint name of the container
	servicePort string                            // the port of the network endpoint for the container
	colonusDir  string                            // the anax side filesystem location for this BC instance
	agreementPH *citizenscientist.ProtocolHandler // CS Protocolhandler for this blockchain client
}

type CSProtocolHandler struct {
	*BaseConsumerProtocolHandler
	genericAgreementPH *citizenscientist.ProtocolHandler
	Work               chan AgreementWork                                // outgoing commands for the workers
	bcState            map[string]map[string]map[string]*BlockchainState // org, name, type
	bcStateLock        sync.Mutex
}

func NewCSProtocolHandler(name string, cfg *config.HorizonConfig, db *bolt.DB, pm *policy.PolicyManager, messages chan events.Message) *CSProtocolHandler {
	if name == citizenscientist.PROTOCOL_NAME {
		return &CSProtocolHandler{
			BaseConsumerProtocolHandler: &BaseConsumerProtocolHandler{
				name:             name,
				pm:               pm,
				db:               db,
				config:           cfg,
				httpClient:       cfg.Collaborators.HTTPClientFactory.NewHTTPClient(nil),
				agbotId:          cfg.AgreementBot.ExchangeId,
				token:            cfg.AgreementBot.ExchangeToken,
				deferredCommands: make([]AgreementWork, 0, 10),
				messages:         messages,
			},
			genericAgreementPH: citizenscientist.NewProtocolHandler(cfg.Collaborators.HTTPClientFactory.NewHTTPClient(nil), pm),
			Work:               make(chan AgreementWork),
			bcState:            make(map[string]map[string]map[string]*BlockchainState),
			bcStateLock:        sync.Mutex{},
		}
	} else {
		return nil
	}
}

func (c *CSProtocolHandler) String() string {
	return fmt.Sprintf("Name: %v, "+
		"PM: %v, "+
		"DB: %v, "+
		"Agreement PH: %v",
		c.Name(), c.pm, c.db, c.genericAgreementPH)
}

func (c *CSProtocolHandler) Initialize() {

	glog.V(5).Infof(CPHlogString(fmt.Sprintf("initializing: %v ", c)))
	// Set up random number gen. This is used to generate agreement id strings.
	random := rand.New(rand.NewSource(int64(time.Now().Nanosecond())))

	// Setup a lock to protect concurrent agreement processing
	agreementLockMgr := NewAgreementLockManager()

	// Set up agreement worker pool based on the current technical config.
	for ix := 0; ix < c.config.AgreementBot.AgreementWorkers; ix++ {
		agw := NewCSAgreementWorker(c, c.config, c.db, c.pm, agreementLockMgr)
		go agw.start(c.Work, random)
	}
	worker.GetWorkerStatusManager().SetWorkerStatus("CSProtocolHandler", worker.STATUS_INITIALIZED)
}

func (c *CSProtocolHandler) AgreementProtocolHandler(typeName string, name string, org string) abstractprotocol.ProtocolHandler {

	if typeName == "" && name == "" && org == "" {
		return c.genericAgreementPH
	}

	c.bcStateLock.Lock()
	defer c.bcStateLock.Unlock()

	nameMap := c.getBCNameMap(org, typeName)
	namedBC, ok := nameMap[name]
	if ok && namedBC.ready {
		return namedBC.agreementPH
	}
	return nil

}

func (c *CSProtocolHandler) WorkQueue() chan AgreementWork {
	return c.Work
}

func (c *CSProtocolHandler) AcceptCommand(cmd worker.Command) bool {

	switch cmd.(type) {
	case *NewProtocolMessageCommand:
		return true
	case *AgreementTimeoutCommand:
		return true
	case *BlockchainEventCommand:
		bcc := cmd.(*BlockchainEventCommand)
		if c.IsBlockchainReady(policy.Ethereum_bc, bcc.Msg.Name(), bcc.Msg.Org()) {
			return true
		} else {
			return false
		}

	case *PolicyChangedCommand:
		return true
	case *PolicyDeletedCommand:
		return true
	case *WorkloadUpgradeCommand:
		return true
	case *MakeAgreementCommand:
		return true
	}
	return false
}

func (c *CSProtocolHandler) PersistAgreement(wi *InitiateAgreement, proposal abstractprotocol.Proposal, workerID string) error {

	var hash, sig = "", ""

	if proposal.Version() == 1 {
		if ag, err := FindSingleAgreementByAgreementId(c.db, proposal.AgreementId(), c.Name(), []AFilter{UnarchivedAFilter()}); err != nil {
			glog.Errorf(CPHlogStringW(workerID, fmt.Sprintf("error retrieving agreement %v from db, error: %v", proposal.AgreementId(), err)))
		} else if ag == nil {
			glog.Errorf(CPHlogStringW(workerID, fmt.Sprintf("cannot find agreement %v from db.", proposal.AgreementId())))
		} else {
			ph := c.AgreementProtocolHandler(ag.BlockchainType, ag.BlockchainName, ag.BlockchainOrg)
			if csph, ok := ph.(*citizenscientist.ProtocolHandler); ok {
				hash, sig, err = csph.SignProposal(proposal)
				if err != nil {
					glog.Errorf(CPHlogStringW(workerID, fmt.Sprintf("error signing proposal %v, error: %v", proposal, err)))
					return err
				}
			} else {
				glog.Errorf(CPHlogStringW(workerID, fmt.Sprintf("for agreement %v, error casting protocol handler to CS protocol handler, is %T", proposal.AgreementId(), ph)))
			}
		}
	}
	return c.BaseConsumerProtocolHandler.PersistBaseAgreement(wi, proposal, workerID, hash, sig)

}

func (c *CSProtocolHandler) PersistReply(r abstractprotocol.ProposalReply, pol *policy.Policy, workerID string) error {

	if reply, ok := r.(*citizenscientist.CSProposalReply); !ok {
		return errors.New(CPHlogStringW(workerID, fmt.Sprintf("unable to cast reply %v to %v Proposal Reply, is %T", r, c.Name(), r)))
	} else if _, err := AgreementMade(c.db, reply.AgreementId(), reply.Address, reply.Signature, c.Name(), pol.HAGroup.Partners, reply.BlockchainType, reply.BlockchainName, reply.BlockchainOrg); err != nil {
		return errors.New(CPHlogStringW(workerID, fmt.Sprintf("error updating agreement %v with reply info DB, error: %v", reply.AgreementId(), err)))
	}
	return nil
}

func (c *CSProtocolHandler) HandleBlockchainEvent(cmd *BlockchainEventCommand) {

	glog.V(5).Infof(CPHlogString("received blockchain event."))
	// Unmarshal the raw event
	if csaph, ok := c.AgreementProtocolHandler("", "", "").(*citizenscientist.ProtocolHandler); !ok {
		glog.Errorf(CPHlogString(fmt.Sprintf("unable to cast agreement protocol handler %T to CS specific handler to process BC event %v", c.AgreementProtocolHandler("", "", ""), cmd.Msg.RawEvent())))
	} else if rawEvent, err := csaph.DemarshalEvent(cmd.Msg.RawEvent()); err != nil {
		glog.Errorf(CPHlogString(fmt.Sprintf("unable to demarshal raw event %v, error: %v", cmd.Msg.RawEvent(), err)))
	} else if !csaph.AgreementCreated(rawEvent) && !csaph.ProducerTermination(rawEvent) && !csaph.ConsumerTermination(rawEvent) {
		glog.V(5).Infof(CPHlogString(fmt.Sprintf("ignoring the blockchain event because it is not agreement creation or termination event.")))
	} else {
		agreementId := csaph.GetAgreementId(rawEvent)

		if csaph.AgreementCreated(rawEvent) {
			agreementWork := CSHandleBCRecorded{
				workType:    BC_RECORDED,
				AgreementId: agreementId,
				Protocol:    c.Name(),
			}
			c.Work <- agreementWork
			glog.V(5).Infof(CPHlogString(fmt.Sprintf("queued blockchain agreement recorded event: %v", agreementWork)))

			// If the event is a agreement terminated event
		} else if csaph.ProducerTermination(rawEvent) || csaph.ConsumerTermination(rawEvent) {
			agreementWork := CSHandleBCTerminated{
				workType:    BC_TERMINATED,
				AgreementId: agreementId,
				Protocol:    c.Name(),
			}
			c.Work <- agreementWork
			glog.V(5).Infof(CPHlogString(fmt.Sprintf("queued agreement cancellation due to blockchain termination event: %v", agreementWork)))
		}
	}

}

func (c *CSProtocolHandler) CreateMeteringNotification(mp policy.Meter, ag *Agreement) (*metering.MeteringNotification, error) {

	// This function ASSUMEs that the BC client is already initialized
	myAddress, _ := ethblockchain.AccountId(c.getColonusDir(ag))
	return metering.NewMeteringNotification(mp, ag.AgreementCreationTime, uint64(ag.DataVerificationCheckRate), ag.DataVerificationMissedCount, ag.CurrentAgreementId, ag.ProposalHash, ag.ConsumerProposalSig, myAddress, ag.ProposalSig, "ethereum")
}

func (c *CSProtocolHandler) TerminateAgreement(ag *Agreement, reason uint, workerId string) {
	// The CS protocol doesnt send cancel messages, it depends on the blockchain to maintain the state of
	// any given agreement. This means we can fake up a message target for the TerminateAgreement call
	// because we know that the CS implementation of the agreement protocol wont be sending a message.
	fakeMT := &exchange.ExchangeMessageTarget{
		ReceiverExchangeId:     "",
		ReceiverPublicKeyObj:   nil,
		ReceiverPublicKeyBytes: []byte(""),
		ReceiverMsgEndPoint:    "",
	}
	c.BaseConsumerProtocolHandler.TerminateAgreement(ag, reason, fakeMT, workerId, c)
	glog.V(5).Infof(CPHlogString(fmt.Sprintf("terminated agreement %v", ag.CurrentAgreementId)))
}

func (c *CSProtocolHandler) GetTerminationCode(reason string) uint {
	switch reason {
	case TERM_REASON_POLICY_CHANGED:
		return citizenscientist.AB_CANCEL_POLICY_CHANGED
	case TERM_REASON_NOT_FINALIZED_TIMEOUT:
		return citizenscientist.AB_CANCEL_NOT_FINALIZED_TIMEOUT
	case TERM_REASON_NO_DATA_RECEIVED:
		return citizenscientist.AB_CANCEL_NO_DATA_RECEIVED
	case TERM_REASON_NO_REPLY:
		return citizenscientist.AB_CANCEL_NO_REPLY
	case TERM_REASON_USER_REQUESTED:
		return citizenscientist.AB_USER_REQUESTED
	case TERM_REASON_NEGATIVE_REPLY:
		return citizenscientist.AB_CANCEL_NEGATIVE_REPLY
	case TERM_REASON_CANCEL_DISCOVERED:
		return citizenscientist.AB_CANCEL_DISCOVERED
	case TERM_REASON_CANCEL_FORCED_UPGRADE:
		return citizenscientist.AB_CANCEL_FORCED_UPGRADE
	case TERM_REASON_CANCEL_BC_WRITE_FAILED:
		return citizenscientist.AB_CANCEL_BC_WRITE_FAILED
	case TERM_REASON_NODE_HEARTBEAT:
		return citizenscientist.AB_CANCEL_NODE_HEARTBEAT
	case TERM_REASON_AG_MISSING:
		return citizenscientist.AB_CANCEL_AG_MISSING
	default:
		return 999
	}
}

func (c *CSProtocolHandler) GetTerminationReason(code uint) string {
	return citizenscientist.DecodeReasonCode(uint64(code))
}

func (c *CSProtocolHandler) SetBlockchainClientAvailable(ev *events.BlockchainClientInitializedMessage) {
}

func (c *CSProtocolHandler) SetBlockchainClientNotAvailable(ev *events.BlockchainClientStoppingMessage) {
	c.bcStateLock.Lock()
	defer c.bcStateLock.Unlock()

	nameMap := c.getBCNameMap(ev.BlockchainOrg(), ev.BlockchainType())
	delete(nameMap, ev.BlockchainInstance())
}

func (c *CSProtocolHandler) SetBlockchainWritable(ev *events.AccountFundedMessage) {

	c.bcStateLock.Lock()
	defer c.bcStateLock.Unlock()

	nameMap := c.getBCNameMap(ev.BlockchainOrg(), ev.BlockchainType())

	_, ok := nameMap[ev.BlockchainInstance()]
	if !ok {
		nameMap[ev.BlockchainInstance()] = &BlockchainState{
			ready:       true,
			writable:    true,
			service:     ev.ServiceName(),
			servicePort: ev.ServicePort(),
			colonusDir:  ev.ColonusDir(),
			agreementPH: citizenscientist.NewProtocolHandler(c.httpClient, c.pm),
		}
	} else {
		nameMap[ev.BlockchainInstance()].ready = true
		nameMap[ev.BlockchainInstance()].writable = true
		nameMap[ev.BlockchainInstance()].service = ev.ServiceName()
		nameMap[ev.BlockchainInstance()].servicePort = ev.ServicePort()
		nameMap[ev.BlockchainInstance()].colonusDir = ev.ColonusDir()
		nameMap[ev.BlockchainInstance()].agreementPH = citizenscientist.NewProtocolHandler(c.httpClient, c.pm)
	}

	glog.V(3).Infof(CPHlogString(fmt.Sprintf("initializing agreement protocol handler for %v", ev)))
	if err := nameMap[ev.BlockchainInstance()].agreementPH.InitBlockchain(ev); err != nil {
		glog.Errorf(CPHlogString(fmt.Sprintf("failed initializing CS agreement protocol blockchain handler for %v, error: %v", ev, err)))
	}

	glog.V(3).Infof(CPHlogString(fmt.Sprintf("agreement protocol handler can write to the blockchain now: %v", *nameMap[ev.BlockchainInstance()])))

	c.updateProducers()

}

func (c *CSProtocolHandler) updateProducers() {
	// A filter for limiting the returned set of agreements just to those that are waiting for the BC to come up.
	notYetUpFilter := func() AFilter {
		return func(a Agreement) bool { return a.AgreementProtocolVersion == 2 && a.BCUpdateAckTime == 0 }
	}

	// Find all agreements that are in progress, waiting for the blockchain to come up.
	if agreements, err := FindAgreements(c.db, []AFilter{notYetUpFilter(), UnarchivedAFilter()}, c.Name()); err != nil {
		glog.Errorf(CPHlogString(fmt.Sprintf("failed to get agreements for %v from the database, error: %v", c.Name(), err)))
	} else {

		for _, ag := range agreements {

			// create deferred update command
			c.DeferCommand(AsyncUpdateAgreement{
				workType:    ASYNC_UPDATE,
				AgreementId: ag.CurrentAgreementId,
				Protocol:    c.Name(),
			})

			// create deferred write command
			c.DeferCommand(AsyncWriteAgreement{
				workType:    ASYNC_WRITE,
				AgreementId: ag.CurrentAgreementId,
				Protocol:    c.Name(),
			})
		}

	}
}

func (c *CSProtocolHandler) UpdateProducer(ag *Agreement) {

	glog.V(5).Infof(CPHlogString(fmt.Sprintf("agreement %v can complete agreement protocol", ag.CurrentAgreementId)))

	if _, pubKey, err := c.GetDeviceMessageEndpoint(ag.DeviceId, "workerId"); err != nil {
		glog.Errorf(CPHlogString(fmt.Sprintf("for agreement %v error getting device %v public key, error %v", ag.CurrentAgreementId, ag.DeviceId, err)))
	} else if mt, err := exchange.CreateMessageTarget(ag.DeviceId, nil, pubKey, ""); err != nil {
		glog.Errorf(CPHlogString(fmt.Sprintf("for agreement %v error creating message target %v", ag.CurrentAgreementId, err)))
	} else {
		ph := c.AgreementProtocolHandler(ag.BlockchainType, ag.BlockchainName, ag.BlockchainOrg)
		if csph, ok := ph.(*citizenscientist.ProtocolHandler); !ok {
			glog.Errorf(CPHlogString(fmt.Sprintf("for agreement %v, error casting protocol handler to CS protocol handler, is %T", ag.CurrentAgreementId, ph)))
		} else if err := csph.SendBlockchainConsumerUpdate(ag.CurrentAgreementId, mt, c.GetSendMessage()); err != nil {
			glog.Errorf(CPHlogString(fmt.Sprintf("error sending update for agreement %v, error: %v", ag.CurrentAgreementId, err)))
		} else if proposal, err := csph.DemarshalProposal(ag.Proposal); err != nil {
			glog.Errorf(CPHlogString(fmt.Sprintf("error demarshalling proposal from pending agreement %v, error: %v", ag.CurrentAgreementId, err)))
		} else if hash, sig, err := csph.SignProposal(proposal); err != nil {
			glog.Errorf(CPHlogString(fmt.Sprintf("error signing hash of agreement %v, error: %v", ag.CurrentAgreementId, err)))
		} else if _, err := AgreementBlockchainUpdate(c.db, ag.CurrentAgreementId, sig, hash, "", "", c.Name()); err != nil {
			glog.Errorf(CPHlogString(fmt.Sprintf("error hardening agreement %v hash and signature, error: %v", ag.CurrentAgreementId, err)))
		}
	}

}

func (c *CSProtocolHandler) IsBlockchainWritable(typeName string, name string, org string) bool {

	c.bcStateLock.Lock()
	defer c.bcStateLock.Unlock()

	nameMap := c.getBCNameMap(org, typeName)
	namedBC, ok := nameMap[name]
	if ok && namedBC.ready && namedBC.writable {
		return true
	} else if ok {
		glog.V(5).Infof(CPHlogString(fmt.Sprintf("blockchain type %v state: %v %v", typeName, name, *namedBC)))
	}
	return false

}

func (c *CSProtocolHandler) IsBlockchainReady(typeName string, name string, org string) bool {

	c.bcStateLock.Lock()
	defer c.bcStateLock.Unlock()

	nameMap := c.getBCNameMap(org, typeName)
	namedBC, ok := nameMap[name]
	if ok && namedBC.ready {
		return true
	}
	return false

}

func (c *CSProtocolHandler) CanCancelNow(ag *Agreement) bool {
	if ag == nil {
		return true
	}

	bcType, bcName, bcOrg := c.GetKnownBlockchain(ag)

	c.bcStateLock.Lock()
	defer c.bcStateLock.Unlock()

	nameMap := c.getBCNameMap(bcOrg, bcType)
	namedBC, ok := nameMap[bcName]
	if !ok || (ok && !namedBC.ready) {
		return false
	}

	return true

}

func (c *CSProtocolHandler) getColonusDir(ag *Agreement) string {
	if ag == nil {
		return ""
	}

	bcType, bcName, bcOrg := c.GetKnownBlockchain(ag)

	c.bcStateLock.Lock()
	defer c.bcStateLock.Unlock()

	nameMap := c.getBCNameMap(bcOrg, bcType)
	namedBC, ok := nameMap[bcName]
	if !ok || (ok && !namedBC.ready) {
		return ""
	}

	return nameMap[bcName].colonusDir

}

func (c *CSProtocolHandler) getBCNameMap(org string, typeName string) map[string]*BlockchainState {
	orgMap, ok := c.bcState[org]
	if !ok {
		c.bcState[org] = make(map[string]map[string]*BlockchainState)
		orgMap = c.bcState[org]
	}

	nameMap, ok := orgMap[typeName]
	if !ok {
		orgMap[typeName] = make(map[string]*BlockchainState)
		nameMap = orgMap[typeName]
	}
	return nameMap
}

func (c *CSProtocolHandler) HandleDeferredCommands() {
	cmds := c.BaseConsumerProtocolHandler.GetDeferredCommands()
	for _, aw := range cmds {
		c.Work <- aw
		glog.V(5).Infof(CPHlogString(fmt.Sprintf("queued deferred agreement work %v for a CS worker", aw)))
	}
}

func (c *CSProtocolHandler) PostReply(agreementId string, proposal abstractprotocol.Proposal, reply abstractprotocol.ProposalReply, consumerPolicy *policy.Policy, org string, workerId string) error {

	agreement, err := FindSingleAgreementByAgreementId(c.db, agreementId, c.Name(), []AFilter{UnarchivedAFilter()})
	if err != nil {
		glog.Errorf(CPHlogStringW(workerId, fmt.Sprintf("error querying agreement %v, error: %v", agreementId, err)))
	} else if agreement == nil {
		glog.Errorf(CPHlogStringW(workerId, fmt.Sprintf("cannot find agreement %v from db.", agreementId)))
	} else if agreement.AgreementProtocolVersion < 2 {
		if aph := c.AgreementProtocolHandler(agreement.BlockchainType, agreement.BlockchainName, agreement.BlockchainOrg); aph == nil {
			glog.Errorf(CPHlogStringW(workerId, fmt.Sprintf("for %v agreement protocol handler not ready", agreementId)))
		} else if err := aph.RecordAgreement(proposal, reply, "", "", consumerPolicy, org); err != nil {
			return err
		} else {
			glog.V(3).Infof(CPHlogStringW(workerId, fmt.Sprintf("recorded agreement %v", agreementId)))
		}
	} else if agreement.AgreementProtocolVersion == 2 {

		if c.IsBlockchainWritable(agreement.BlockchainType, agreement.BlockchainName, agreement.BlockchainOrg) {

			// create deferred update command
			c.DeferCommand(AsyncUpdateAgreement{
				workType:    ASYNC_UPDATE,
				AgreementId: agreement.CurrentAgreementId,
				Protocol:    c.Name(),
			})

			// create deferred write command
			c.DeferCommand(AsyncWriteAgreement{
				workType:    ASYNC_WRITE,
				AgreementId: agreement.CurrentAgreementId,
				Protocol:    c.Name(),
			})

		} else {
			c.messages <- events.NewNewBCContainerMessage(events.NEW_BC_CLIENT, agreement.BlockchainType, agreement.BlockchainName, agreement.BlockchainOrg, c.config.AgreementBot.ExchangeURL, c.agbotId, c.token)
		}
	}
	return nil

}

func (c *CSProtocolHandler) HandleExtensionMessage(cmd *NewProtocolMessageCommand) error {

	glog.V(5).Infof(CPHlogString(fmt.Sprintf("received inbound exchange message.")))
	// Figure out what kind of message this is
	if update, perr := c.genericAgreementPH.ValidateBlockchainProducerUpdate(string(cmd.Message)); perr == nil {
		agreementWork := CSProducerUpdate{
			workType:     PRODUCER_UPDATE,
			Update:       *update,
			SenderId:     cmd.From,
			SenderPubKey: cmd.PubKey,
			MessageId:    cmd.MessageId,
		}
		c.WorkQueue() <- agreementWork
		glog.V(5).Infof(CPHlogString(fmt.Sprintf("queued producer update message")))

	} else if updateAck, aerr := c.genericAgreementPH.ValidateBlockchainConsumerUpdateAck(string(cmd.Message)); aerr == nil {
		agreementWork := CSConsumerUpdateAck{
			workType:     CONSUMER_UPDATE_ACK,
			Update:       *updateAck,
			SenderId:     cmd.From,
			SenderPubKey: cmd.PubKey,
			MessageId:    cmd.MessageId,
		}
		c.WorkQueue() <- agreementWork
		glog.V(5).Infof(CPHlogString(fmt.Sprintf("queued consumer update ack message")))

	} else {
		glog.V(5).Infof(CPHlogString(fmt.Sprintf("ignoring  message: %v because it is an unknown type", string(cmd.Message))))
		return errors.New(CPHlogString(fmt.Sprintf("unknown protocol msg %s", cmd.Message)))
	}
	return nil

}

func (c *CSProtocolHandler) AlreadyReceivedReply(ag *Agreement) bool {
	if (ag.AgreementProtocolVersion < 2 && ag.CounterPartyAddress != "") || (ag.AgreementProtocolVersion == 2 && ag.BlockchainType != "") {
		return true
	}
	return false
}

func (c *CSProtocolHandler) GetKnownBlockchain(ag *Agreement) (string, string, string) {
	return ag.BlockchainType, ag.BlockchainName, ag.BlockchainOrg
}

func (c *CSProtocolHandler) CanSendMeterRecord(ag *Agreement) bool {
	return ag.ProposalSig != "" && ag.ConsumerProposalSig != ""
}

// ==========================================================================================================
// Utility functions

var CPHlogString = func(v interface{}) string {
	return fmt.Sprintf("AgreementBot CS Protocol Handler %v", v)
}

var CPHlogStringW = func(workerId string, v interface{}) string {
	return fmt.Sprintf("AgreementBot CS Protocol Handler (%v) %v", workerId, v)
}
