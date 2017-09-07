package stake

import (
	"fmt"
	"strconv"

	abci "github.com/tendermint/abci/types"
	"github.com/tendermint/go-wire"
	"github.com/tendermint/tmlibs/log"

	"github.com/cosmos/cosmos-sdk"
	"github.com/cosmos/cosmos-sdk/errors"
	"github.com/cosmos/cosmos-sdk/modules/auth"
	"github.com/cosmos/cosmos-sdk/modules/base"
	"github.com/cosmos/cosmos-sdk/modules/coin"
	"github.com/cosmos/cosmos-sdk/modules/fee"
	"github.com/cosmos/cosmos-sdk/modules/ibc"
	"github.com/cosmos/cosmos-sdk/modules/nonce"
	"github.com/cosmos/cosmos-sdk/modules/roles"
	"github.com/cosmos/cosmos-sdk/stack"
	"github.com/cosmos/cosmos-sdk/state"
)

//nolint
const (
	name = "stake"

	queueUnbondTB = iota
	queueCommissionTB
)

//nolint
var (
	Period2Unbond uint64 = 30     // queue blocks before unbond
	CoinDenom     string = "atom" // bondable coin denomination

	MaxGroupCommChange        = newDecimal(5, -2) //maximum commission change permitted across the previous ModCommCheckBlocks blocks
	ModCommCheckBlocks uint64 = 28800             //1 day at 1 block/3 sec

	Inflation Decimal = NewDecimal(7, -2) // inflation between (0 to 1)
)

// Name - simply the name TODO do we need name to be unexposed for security?
func Name() string {
	return name
}

// NewHandler returns a new counter transaction processing handler
func NewHandler(feeDenom string) sdk.Handler {
	return stack.New(
		base.Logger{},
		stack.Recovery{},
		auth.Signatures{},
		base.Chain{},
		stack.Checkpoint{OnCheck: true},
		nonce.ReplayCheck{},
	).
		IBC(ibc.NewMiddleware()).
		Apps(
			roles.NewMiddleware(),
			fee.NewSimpleFeeMiddleware(coin.Coin{feeDenom, 0}, fee.Bank),
			stack.Checkpoint{OnDeliver: true},
		).
		Dispatch(
			coin.NewHandler(),
			stack.WrapHandler(roles.NewHandler()),
			stack.WrapHandler(ibc.NewHandler()),
		)
}

// Handler the transaction processing handler
type Handler struct {
	stack.PassInitValidate
}

var _ stack.Dispatchable = Handler{} //enforce interface at compile time

// Name - return stake namespace
func (Handler) Name() string {
	return name
}

// AssertDispatcher - placeholder for stack.Dispatchable
func (Handler) AssertDispatcher() {}

// InitState - set genesis parameters for staking
func (Handler) InitState(l log.Logger, store state.SimpleDB,
	module, key, value string, cb sdk.InitStater) (log string, err error) {
	if module != name {
		return "", errors.ErrUnknownModule(module)
	}
	switch key {
	case "unbond_period":
		period, err := strconv.Atoi(value)
		if err != nil {
			return "", fmt.Errorf("unbond period must be int, Error: %v", err.Error())
		}
		Period2Unbond = uint64(period)
	case "modcomm_period":
		period, err := strconv.Atoi(value)
		if err != nil {
			return "", fmt.Errorf("modcomm period must be int, Error: %v", err.Error())
		}
		Period2ModComm = uint64(period)
	case "bond_coin":
		CoinDenom = value
	}
	return "", errors.ErrUnknownKey(key)
}

// CheckTx checks if the tx is properly structured
func (h Handler) CheckTx(ctx sdk.Context, store state.SimpleDB,
	tx sdk.Tx, _ sdk.Checker) (res sdk.CheckResult, err error) {
	err = checkTx(ctx, tx)

	return
}
func checkTx(ctx sdk.Context, tx sdk.Tx) (err error) {
	err = tx.Unwrap().ValidateBasic()
	return
}

// DeliverTx executes the tx if valid
func (h Handler) DeliverTx(ctx sdk.Context, store state.SimpleDB,
	tx sdk.Tx, dispatch sdk.Deliver) (res sdk.DeliverResult, err error) {
	err = checkTx(ctx, tx)
	if err != nil {
		return
	}

	//start by processing the unbonding queue
	height := ctx.BlockHeight()
	err = processQueueUnbond(ctx, store, height, dispatch)
	if err != nil {
		return
	}
	err = processQueueModComm(ctx, store, height)
	if err != nil {
		return
	}
	err = processValidatorRewards(ctx, store, height, dispatch)
	if err != nil {
		return
		return
	}

	//now actually run the transaction
	unwrap := tx.Unwrap()
	var abciRes abci.Result
	switch txType := unwrap.(type) {
	case TxBond:
		abciRes = runTxBond(ctx, store, txType, dispatch)
	case TxUnbond:
		abciRes = runTxUnbond(ctx, store, txType, height)
	case TxNominate:
		abciRes = runTxNominate(ctx, store, txType, dispatch)
	case TxModComm:
		abciRes = runTxModComm(ctx, store, txType, height)
	}

	//determine the validator set changes
	delegateeBonds, err := getDelegateeBonds(store)
	if err != nil {
		return res, err
	}
	res = sdk.DeliverResult{
		Data:    abciRes.Data,
		Log:     abciRes.Log,
		Diff:    delegateeBonds.ValidatorsDiff(nil), //TODO add the previous validator set instead of nil
		GasUsed: 0,                                  //TODO add gas accounting
	}
	return
}

///////////////////////////////////////////////////////////////////////////////////////////////////

func runTxBond(ctx sdk.Context, store state.SimpleDB, tx TxBond,
	dispatch sdk.Deliver) (res abci.Result) {

	// Get amount of coins to bond
	bondCoin := tx.Amount
	bondAmt := NewDecimal(bondCoin.Amount, 1)

	switch {
	case bondCoin.Denom != CoinDenom:
		return abci.ErrInternalError.AppendLog("Invalid coin denomination")
	case bondAmt.LTE(Zero):
		return abci.ErrInternalError.AppendLog("Amount must be > 0")
	}

	// Get the delegatee bond account
	delegateeBonds, err := getDelegateeBonds(store)
	if err != nil {
		return abci.ErrInternalError.AppendLog(err.Error())
	}
	_, delegateeBond := delegateeBonds.Get(tx.Delegatee)
	if delegateeBond == nil {
		return abci.ErrInternalError.AppendLog("Cannot bond to non-nominated account")
	}

	// Move coins from the deletatee account to the delegatee lock account
	senders := ctx.GetPermissions("", auth.NameSigs) //XXX does auth need to be checked here?
	if len(senders) != 1 {
		return abci.ErrInternalError.AppendLog("Missing signature")
	}
	sender := senders[0]
	send := coin.NewSendOneTx(sender, delegateeBond.Account, coin.Coins{bondCoin})

	// If the deduction fails (too high), abort the command
	_, err = dispatch.DeliverTx(ctx, store, send)
	if err != nil {
		return abci.ErrInternalError.AppendLog(err.Error())
	}

	// Get or create delegator account
	delegatorBonds, err := getDelegatorBonds(store, sender)
	if err != nil {
		return abci.ErrInternalError.AppendLog(err.Error())
	}
	if len(delegatorBonds) != 1 {
		delegatorBonds = DelegatorBonds{
			DelegatorBond{
				Delegatee:  tx.Delegatee,
				BondTokens: Zero,
			},
		}
	}

	// Calculate amount of bond tokens to create, based on exchange rate
	bondTokens := bondAmt.Div(delegateeBond.ExchangeRate)
	delegatorBonds[0].BondTokens = delegatorBonds[0].BondTokens.Plus(bondTokens)

	// Save to store
	setDelegateeBonds(store, delegateeBonds)
	setDelegatorBonds(store, sender, delegatorBonds)

	return abci.OK
}

func runTxUnbond(ctx sdk.Context, store state.SimpleDB, tx TxUnbond,
	height uint64) (res abci.Result) {

	bondAmt := NewDecimal(tx.Amount.Amount, 1)

	if bondAmt.LTE(Zero) {
		return abci.ErrInternalError.AppendLog("Unbond amount must be > 0")
	}

	senders := ctx.GetPermissions("", auth.NameSigs) //XXX does auth need to be checked here?
	if len(senders) != 0 {
		return abci.ErrInternalError.AppendLog("Missing signature")
	}
	sender := senders[0]

	delegatorBonds, err := getDelegatorBonds(store, sender)
	if err != nil {
		return abci.ErrInternalError.AppendLog(err.Error())
	}
	if delegatorBonds == nil {
		return abci.ErrBaseUnknownAddress.AppendLog("No bond account for this (address, validator) pair")
	}
	_, delegatorBond := delegatorBonds.Get(tx.Delegatee)
	if delegatorBond == nil {
		return abci.ErrInternalError.AppendLog("Delegator does not contain delegatee bond")
	}

	// subtract bond tokens from bond account
	if delegatorBond.BondTokens.LT(bondAmt) {
		return abci.ErrBaseInsufficientFunds.AppendLog("Insufficient bond tokens")
	}
	delegatorBond.BondTokens = delegatorBond.BondTokens.Minus(bondAmt)
	//New exchange rate = (new number of bonded atoms)/ total number of bondTokens for validator
	//delegateeBond.ExchangeRate := uint64(bondAmt) / bondTokens

	if delegatorBond.BondTokens.Equal(Zero) {
		removeDelegatorBonds(store, sender)
	} else {
		setDelegatorBonds(store, sender, delegatorBonds)
	}

	// subtract tokens from bond value
	delegateeBonds, err := getDelegateeBonds(store)
	if err != nil {
		return abci.ErrInternalError.AppendLog(err.Error())
	}
	bvIndex, delegateeBond := delegateeBonds.Get(tx.Delegatee)
	if delegatorBond == nil {
		return abci.ErrInternalError.AppendLog("Delegatee does not exist for that address")
	}
	delegateeBond.TotalBondTokens = delegateeBond.TotalBondTokens.Minus(bondAmt)
	if delegateeBond.TotalBondTokens.Equal(Zero) {
		delegateeBonds.Remove(bvIndex)
	}
	setDelegateeBonds(store, delegateeBonds)
	// TODO Delegatee bonds?

	// add unbond record to queue
	queueElem := QueueElemUnbond{
		QueueElem: QueueElem{
			Delegatee:    tx.Delegatee,
			HeightAtInit: height, // will unbond at `height + Period2Unbond`
		},
		Account:    sender,
		BondTokens: bondAmt,
	}
	queue, err := LoadQueue(queueUnbondTB, store)
	if err != nil {
		return abci.ErrInternalError.AppendLog(err.Error())
	}
	bytes := wire.BinaryBytes(queueElem)
	queue.Push(bytes)

	return abci.OK
}

func runTxNominate(ctx sdk.Context, store state.SimpleDB, tx TxNominate,
	dispatch sdk.Deliver) (res abci.Result) {

	// Create bond value object
	delegateeBond := DelegateeBond{
		Delegatee:    tx.Nominee,
		Commission:   tx.Commission,
		ExchangeRate: One,
	}

	// Bond the tokens
	senders := ctx.GetPermissions("", auth.NameSigs) //XXX does auth need to be checked here?
	if len(senders) == 0 {
		return abci.ErrInternalError.AppendLog("Missing signature")
	}
	send := coin.NewSendOneTx(senders[0], delegateeBond.Account, coin.Coins{tx.Amount})
	_, err := dispatch.DeliverTx(ctx, store, send)
	if err != nil {
		return abci.ErrInternalError.AppendLog(err.Error())
	}

	// Append and store
	delegateeBonds, err := getDelegateeBonds(store)
	if err != nil {
		return abci.ErrInternalError.AppendLog(err.Error())
	}
	delegateeBonds = append(delegateeBonds, delegateeBond)
	setDelegateeBonds(store, delegateeBonds)

	return abci.OK
}

//TODO Update logic
func runTxModComm(ctx sdk.Context, store state.SimpleDB, tx TxModComm,
	height uint64) (res abci.Result) {

	// Retrieve the record to modify
	delegateeBonds, err := getDelegateeBonds(store)
	if err != nil {
		return abci.ErrInternalError.AppendLog(err.Error())
	}
	_, delegateeBond := delegateeBonds.Get(tx.Delegatee)
	if delegateeBond == nil {
		return abci.ErrInternalError.AppendLog("Delegatee does not exist for that address")
	}

	// Add the commission modification the queue
	queueElem := QueueElemModComm{
		QueueElem: QueueElem{
			Delegatee:    tx.Delegatee,
			HeightAtInit: height, // will unbond at `height + Period2Unbond`
		},
		Commission: tx.Commission,
	}
	queue, err := LoadQueue(queueCommissionTB, store)
	if err != nil {
		return abci.ErrInternalError.AppendLog(err.Error())
	}
	bytes := wire.BinaryBytes(queueElem)
	queue.Push(bytes)

	return abci.OK
}

/////////////////////////////////////////////////////////////////////////////////////////////////////
// Process all unbonding for the current block, note that the unbonding amounts
//   have already been subtracted from the bond account when they were added to the queue
func processQueueUnbond(ctx sdk.Context, store state.SimpleDB,
	height uint64, dispatch sdk.Deliver) error {
	queue, err := LoadQueue(queueUnbondTB, store)
	if err != nil {
		return err
	}

	//Get the peek unbond record from the queue
	var unbond QueueElemUnbond
	getUnbond := func() error {
		unbondBytes := queue.Peek()
		return wire.ReadBinaryBytes(unbondBytes, unbond)
	}
	err = getUnbond()
	if err != nil {
		return err
	}

	for unbond.Delegatee.Address != nil && height-unbond.HeightAtInit > Period2Unbond {
		queue.Pop()

		// send unbonded coins to queue account, based on current exchange rate
		delegateeBonds, err := getDelegateeBonds(store)
		if err != nil {
			return err
		}
		_, delegateeBond := delegateeBonds.Get(unbond.Delegatee)
		if delegateeBond == nil {
			return abci.ErrInternalError.AppendLog("Delegatee does not exist for that address")
		}
		coinAmount := unbond.BondTokens.Mul(delegateeBond.ExchangeRate)
		payout := coin.Coins{{CoinDenom, coinAmount.IntPart()}} //TODO here coins must also be decimal!!!!

		send := coin.NewSendOneTx(delegateeBond.Account, unbond.Account, payout)
		_, err = dispatch.DeliverTx(ctx, store, send)
		if err != nil {
			return err
		}

		// get next unbond record
		err = getUnbond()
		if err != nil {
			return err
		}
	}
	return nil
}

// TODO modComm nolonger uses the queue should remove this function all together
// Process all validator commission modification for the current block
func processQueueModComm(ctx sdk.Context, store state.SimpleDB, height uint64) error {
	queue, err := LoadQueue(queueCommissionTB, store)
	if err != nil {
		return err
	}

	//Get the peek record from the queue
	var commission QueueElemModComm
	getCommission := func() error {
		bytes := queue.Peek()
		return wire.ReadBinaryBytes(bytes, commission)
	}
	err = getCommission()
	if err != nil {
		return err
	}

	for commission.Delegatee.Address != nil && height-commission.HeightAtInit > Period2ModComm {
		queue.Pop()

		// Retrieve, Modify and save the commission
		delegateeBonds, err := getDelegateeBonds(store)
		if err != nil {
			return err
		}
		record, _ := delegateeBonds.Get(commission.Delegatee)
		if err != nil {
			return err
		}
		delegateeBonds[record].Commission = commission.Commission
		setDelegateeBonds(store, delegateeBonds)

		// check the next record in the queue record
		err = getCommission()
		if err != nil {
			return err
		}
	}
	return nil
}

//TODO add processing of the commission
func processValidatorRewards(ctx sdk.Context, store state.SimpleDB,
	height uint64, dispatch sdk.Deliver) error {

	// Retrieve the list of validators
	delegateeBonds, err := getDelegateeBonds(store)
	if err != nil {
		return err
	}
	validatorAccounts := delegateeBonds.ValidatorsActors()

	for _, account := range validatorAccounts {

		credit := coin.Coins{{"atom", 10}} //TODO update to relative to the amount of coins held by validator

		creditTx := coin.NewCreditTx(account, credit)
		_, err = dispatch.DeliverTx(ctx, store, creditTx)
		if err != nil {
			return err
		}
	}
	return nil
}
