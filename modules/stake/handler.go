package stake

import (
	"fmt"
	"strconv"

	"github.com/tendermint/tmlibs/log"

	"github.com/cosmos/cosmos-sdk"
	"github.com/cosmos/cosmos-sdk/errors"
	"github.com/cosmos/cosmos-sdk/modules/auth"
	"github.com/cosmos/cosmos-sdk/modules/coin"
	"github.com/cosmos/cosmos-sdk/stack"
	"github.com/cosmos/cosmos-sdk/state"
)

// nolint
const stakingModuleName = "stake"

// Name is the name of the modules.
func Name() string {
	return stakingModuleName
}

//_______________________________________________________________________

// DelegatedProofOfStake - interface to enforce delegation stake
type DelegatedProofOfStake interface {
	declareCandidacy(TxDeclareCandidacy) error
	editCandidacy(TxEditCandidacy) error
	delegate(TxDelegate) error
	unbond(TxUnbond) error
}

type checker struct {
	store  state.SimpleDB
	sender sdk.Actor
}

type deliverer struct {
	store      state.SimpleDB
	sender     sdk.Actor
	params     Params
	transferFn transferFn
}

var _, _ DelegatedProofOfStake = deliverer{}, checker{} // enforce interface at compile time
//_______________________________________________________________________

// Handler - the transaction processing handler
type Handler struct {
	stack.PassInitValidate
}

var _ stack.Dispatchable = Handler{} // enforce interface at compile time

// NewHandler returns a new Handler with the default Params
func NewHandler() Handler {
	return Handler{}
}

// Name - return stake namespace
func (Handler) Name() string {
	return stakingModuleName
}

// AssertDispatcher - placeholder for stack.Dispatchable
func (Handler) AssertDispatcher() {}

// InitState - set genesis parameters for staking
func (h Handler) InitState(l log.Logger, store state.SimpleDB,
	module, key, value string, cb sdk.InitStater) (log string, err error) {
	return "", h.initState(module, key, value, store)
}

// separated for testing
func (Handler) initState(module, key, value string, store state.SimpleDB) error {
	if module != stakingModuleName {
		return errors.ErrUnknownModule(module)
	}

	params := loadParams(store)
	switch key {
	case "allowed_bond_denom":
		params.AllowedBondDenom = value
	case "max_vals",
		"gas_bond",
		"gas_unbond":

		// TODO: enforce non-negative integers in input
		i, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("input must be integer, Error: %v", err.Error())
		}

		switch key {
		case "max_vals":
			params.MaxVals = uint16(i)
		case "gas_bond":
			params.GasDelegate = uint64(i)
		case "gas_unbound":
			params.GasUnbond = uint64(i)
		}
	default:
		return errors.ErrUnknownKey(key)
	}

	saveParams(store, params)
	return nil
}

// CheckTx checks if the tx is properly structured
func (h Handler) CheckTx(ctx sdk.Context, store state.SimpleDB,
	tx sdk.Tx, _ sdk.Checker) (res sdk.CheckResult, err error) {

	err = tx.ValidateBasic()
	if err != nil {
		return res, err
	}

	// get the sender
	sender, err := getTxSender(ctx)
	if err != nil {
		return res, err
	}

	params := loadParams(store)

	// create the new checker object to
	checker := checker{
		store:  store,
		sender: sender,
	}

	// return the fee for each tx type
	switch txInner := tx.Unwrap().(type) {
	case TxDeclareCandidacy:
		return sdk.NewCheck(params.GasDeclareCandidacy, ""),
			checker.declareCandidacy(txInner)
	case TxEditCandidacy:
		return sdk.NewCheck(params.GasEditCandidacy, ""),
			checker.editCandidacy(txInner)
	case TxDelegate:
		return sdk.NewCheck(params.GasDelegate, ""),
			checker.delegate(txInner)
	case TxUnbond:
		return sdk.NewCheck(params.GasUnbond, ""),
			checker.unbond(txInner)
	}

	return res, errors.ErrUnknownTxType(tx)
}

func (c checker) declareCandidacy(tx TxDeclareCandidacy) error {

	// check to see if the pubkey or sender has been registered before
	candidate := loadCandidate(c.store, tx.PubKey)
	if candidate != nil {
		return fmt.Errorf("cannot bond to pubkey which is already declared candidacy"+
			" PubKey %v already registered with %v candidate address",
			candidate.PubKey, candidate.Owner)
	}

	return checkDenom(tx.BondUpdate, c.store)
}

func (c checker) editCandidacy(tx TxEditCandidacy) error {

	// candidate must already be registered
	candidate := loadCandidate(c.store, tx.PubKey)
	if candidate == nil { // does PubKey exist
		return fmt.Errorf("cannot delegate to non-existant PubKey %v", tx.PubKey)
	}
	return nil
}

func (c checker) delegate(tx TxDelegate) error {

	candidate := loadCandidate(c.store, tx.PubKey)
	if candidate == nil { // does PubKey exist
		return fmt.Errorf("cannot delegate to non-existant PubKey %v", tx.PubKey)
	}
	return checkDenom(tx.BondUpdate, c.store)
}

func (c checker) unbond(tx TxUnbond) error {

	// check if have enough shares to unbond
	bond := loadDelegatorBond(c.store, c.sender, tx.PubKey)
	if bond.Shares < tx.Shares {
		return fmt.Errorf("not enough bond shares to unbond, have %v, trying to unbond %v",
			bond.Shares, tx.Shares)
	}
	return nil
}

func checkDenom(tx BondUpdate, store state.SimpleDB) error {
	if tx.Bond.Denom != loadParams(store).AllowedBondDenom {
		return fmt.Errorf("Invalid coin denomination")
	}
	return nil
}

// DeliverTx executes the tx if valid
func (h Handler) DeliverTx(ctx sdk.Context, store state.SimpleDB,
	tx sdk.Tx, dispatch sdk.Deliver) (res sdk.DeliverResult, err error) {

	// TODO remove nessesity for this defer (and used function)
	//defer updateVotingPower(store)

	// TODO: remove redundancy
	// also we don't need to check the res - gas is already deducted in sdk
	_, err = h.CheckTx(ctx, store, tx, nil)
	if err != nil {
		return
	}

	sender, err := getTxSender(ctx)
	if err != nil {
		return
	}

	params := loadParams(store)
	fn := defaultTransferFn(ctx, store, dispatch)
	deliverer := deliverer{
		store:      store,
		sender:     sender,
		params:     params,
		transferFn: fn,
	}

	// Run the transaction
	switch _tx := tx.Unwrap().(type) {
	case TxDeclareCandidacy:
		res.GasUsed = params.GasDeclareCandidacy
		return res, deliverer.declareCandidacy(_tx)
	case TxEditCandidacy:
		res.GasUsed = params.GasEditCandidacy
		return res, deliverer.editCandidacy(_tx)
	case TxDelegate:
		res.GasUsed = params.GasDelegate
		return res, deliverer.delegate(_tx)
	case TxUnbond:
		//context with hold account permissions
		params := loadParams(store)
		res.GasUsed = params.GasUnbond
		ctx2 := ctx.WithPermissions(params.HoldAccount)
		deliverer.transferFn = defaultTransferFn(ctx2, store, dispatch)
		return res, deliverer.unbond(_tx)
	}
	return
}

//---------------------------------------------------------------------
// These functions assume everything has been authenticated,
// now we just perform action and save

// TODO: why not just return (sdk.DeliverResult, error)?
// that is why the other interface is such, and err != nil
// is more idiomatic than res.IsErr()
func (d deliverer) declareCandidacy(tx TxDeclareCandidacy) error {

	// create and save the empty candidate
	bond := loadCandidate(d.store, tx.PubKey)
	if bond != nil {
		return ErrCandidateExistsAddr()
	}
	candidate := NewCandidate(tx.PubKey, d.sender)
	candidate.Description = tx.Description // add the description parameters
	saveCandidate(d.store, candidate)

	// move coins from the d.sender account to a (self-bond) delegator account
	// the candidate account will be updated automatically here
	txDelegate := TxDelegate{tx.BondUpdate}
	return d.delegate(txDelegate)
}

func (d deliverer) editCandidacy(tx TxEditCandidacy) error {

	// Get the pubKey bond account
	candidate := loadCandidate(d.store, tx.PubKey)
	if candidate == nil {
		return ErrBondNotNominated()
	}
	if candidate.Owner.Empty() { //candidate has been withdrawn
		return ErrBondNotNominated()
	}

	//check and edit any of the editable terms
	if tx.Description.Moniker != "" {
		candidate.Description.Moniker = tx.Description.Moniker
	}
	if tx.Description.Identity != "" {
		candidate.Description.Identity = tx.Description.Identity
	}
	if tx.Description.Website != "" {
		candidate.Description.Website = tx.Description.Website
	}
	if tx.Description.Details != "" {
		candidate.Description.Details = tx.Description.Details
	}

	saveCandidate(d.store, candidate)
	return nil
}

func (d deliverer) delegate(tx TxDelegate) error {

	// Get the pubKey bond account
	candidate := loadCandidate(d.store, tx.PubKey)
	if candidate == nil {
		return ErrBondNotNominated()
	}
	if candidate.Owner.Empty() { //candidate has been withdrawn
		return ErrBondNotNominated()
	}

	// Move coins from the delegator account to the pubKey lock account
	err := d.transferFn(d.sender, d.params.HoldAccount, coin.Coins{tx.Bond})
	if err != nil {
		return err
	}
	//key := stack.PrefixedKey(coin.NameCoin, d.sender.Address)
	//acc := coin.Account{}
	//query.GetParsed(key, &acc, query.GetHeight(), false)
	//panic(fmt.Sprintf("debug acc: %v\n", acc))

	// Get or create the delegator bond
	bond := loadDelegatorBond(d.store, d.sender, tx.PubKey)
	if bond == nil {
		bond = &DelegatorBond{
			PubKey: tx.PubKey,
			Shares: 0,
		}
	}

	// Add shares to delegator bond and candidate
	bondAmount := uint64(tx.Bond.Amount) // XXX: checked for underflow in ValidateBasic
	bond.Shares += bondAmount
	candidate.Shares += bondAmount

	// Save to d.store
	saveCandidate(d.store, candidate)
	saveDelegatorBond(d.store, d.sender, bond)

	return nil
}

func (d deliverer) unbond(tx TxUnbond) error {

	// get delegator bond
	bond := loadDelegatorBond(d.store, d.sender, tx.PubKey)
	if bond == nil {
		return ErrNoDelegatorForAddress()
	}

	// get pubKey candidate
	candidate := loadCandidate(d.store, tx.PubKey)
	if candidate == nil {
		return ErrNoCandidateForAddress()
	}

	// subtract bond tokens from bond
	if bond.Shares < tx.Shares {
		return ErrInsufficientFunds()
	}
	bond.Shares -= tx.Shares

	if bond.Shares == 0 {

		// if the bond is the owner of the candidate then
		// trigger a reject candidacy by setting Owner to Empty Actor
		if d.sender.Equals(candidate.Owner) {
			candidate.Owner = sdk.Actor{}
		}

		// remove the bond
		removeDelegatorBond(d.store, d.sender, tx.PubKey)
	} else {
		saveDelegatorBond(d.store, d.sender, bond)
	}

	// deduct shares from the candidate
	candidate.Shares -= tx.Shares
	if candidate.Shares == 0 {
		removeCandidate(d.store, tx.PubKey)
	} else {
		saveCandidate(d.store, candidate)
	}

	// transfer coins back to account
	txShares := int64(tx.Shares) // XXX: watch overflow
	returnCoins := txShares      //currently each share is worth one coin
	return d.transferFn(d.params.HoldAccount, d.sender,
		coin.Coins{{d.params.AllowedBondDenom, returnCoins}})
}

// get the sender from the ctx and ensure it matches the tx pubkey
func getTxSender(ctx sdk.Context) (sender sdk.Actor, err error) {
	senders := ctx.GetPermissions("", auth.NameSigs)
	if len(senders) != 1 {
		return sender, ErrMissingSignature()
	}
	return senders[0], nil
}
