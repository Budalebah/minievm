package keeper

import (
	"context"
	"fmt"

	"cosmossdk.io/core/store"

	"github.com/cosmos/cosmos-sdk/codec"
	"github.com/cosmos/cosmos-sdk/telemetry"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	cosmosbank "github.com/cosmos/cosmos-sdk/x/bank/keeper"
	"github.com/cosmos/cosmos-sdk/x/bank/types"

	evmtypes "github.com/initia-labs/minievm/x/evm/types"
)

var _ cosmosbank.SendKeeper = (*EVMSendKeeper)(nil)

// EVMSendKeeper only allows transfers between accounts without the possibility of
// creating coins. It implements the SendKeeper interface.
type EVMSendKeeper struct {
	EVMViewKeeper

	cdc          codec.BinaryCodec
	ak           types.AccountKeeper
	storeService store.KVStoreService

	// list of addresses that are restricted from receiving transactions
	blockedAddrs map[string]bool

	// the address capable of executing a MsgUpdateParams message. Typically, this
	// should be the x/gov module account.
	authority string

	sendRestriction *sendRestriction
}

func NewEVMSendKeeper(
	cdc codec.BinaryCodec,
	storeService store.KVStoreService,
	ak types.AccountKeeper,
	ek evmtypes.IERC20Keeper,
	blockedAddrs map[string]bool,
	authority string,
) EVMSendKeeper {
	if _, err := ak.AddressCodec().StringToBytes(authority); err != nil {
		panic(fmt.Errorf("invalid bank authority address: %w", err))
	}

	return EVMSendKeeper{
		EVMViewKeeper:   NewEVMViewKeeper(cdc, storeService, ak, ek),
		cdc:             cdc,
		ak:              ak,
		storeService:    storeService,
		blockedAddrs:    blockedAddrs,
		authority:       authority,
		sendRestriction: newSendRestriction(),
	}
}

// AppendSendRestriction adds the provided SendRestrictionFn to run after previously provided restrictions.
func (k EVMSendKeeper) AppendSendRestriction(restriction types.SendRestrictionFn) {
	k.sendRestriction.append(restriction)
}

// PrependSendRestriction adds the provided SendRestrictionFn to run before previously provided restrictions.
func (k EVMSendKeeper) PrependSendRestriction(restriction types.SendRestrictionFn) {
	k.sendRestriction.prepend(restriction)
}

// ClearSendRestriction removes the send restriction (if there is one).
func (k EVMSendKeeper) ClearSendRestriction() {
	k.sendRestriction.clear()
}

// GetAuthority returns the x/bank module's authority.
func (k EVMSendKeeper) GetAuthority() string {
	return k.authority
}

// GetParams returns the total set of bank parameters.
func (k EVMSendKeeper) GetParams(ctx context.Context) (params types.Params) {
	p, _ := k.Params.Get(ctx)
	return p
}

// SetParams sets the total set of bank parameters.
//
// Note: params.SendEnabled is deprecated but it should be here regardless.
//
//nolint:staticcheck
func (k EVMSendKeeper) SetParams(ctx context.Context, params types.Params) error {
	// Normally SendEnabled is deprecated but we still support it for backwards
	// compatibility. Using params.Validate() would fail due to the SendEnabled
	// deprecation.
	if len(params.SendEnabled) > 0 { //nolint:staticcheck // SA1019: params.SendEnabled is deprecated
		k.SetAllSendEnabled(ctx, params.SendEnabled) //nolint:staticcheck // SA1019: params.SendEnabled is deprecated

		// override params without SendEnabled
		params = types.NewParams(params.DefaultSendEnabled)
	}
	return k.Params.Set(ctx, params)
}

// InputOutputCoins performs multi-send functionality. It accepts a series of
// inputs that correspond to a series of outputs. It returns an error if the
// inputs and outputs don't lineup or if any single transfer of tokens fails.
func (k EVMSendKeeper) InputOutputCoins(ctx context.Context, inputs types.Input, outputs []types.Output) error {
	return sdkerrors.ErrNotSupported
}

// SendCoins transfers amt coins from a sending account to a receiving account.
// An error is returned upon failure.
func (k EVMSendKeeper) SendCoins(ctx context.Context, fromAddr sdk.AccAddress, toAddr sdk.AccAddress, amt sdk.Coins) error {
	toAddr, err := k.sendRestriction.apply(ctx, fromAddr, toAddr, amt)
	if err != nil {
		return err
	}

	err = k.ek.SendCoins(ctx, fromAddr, toAddr, amt)
	if err != nil {
		return err
	}

	sdkCtx := sdk.UnwrapSDKContext(ctx)

	// emit coin spent event
	sdkCtx.EventManager().EmitEvent(
		types.NewCoinSpentEvent(fromAddr, amt),
	)

	// emit coin received event
	sdkCtx.EventManager().EmitEvent(
		types.NewCoinReceivedEvent(toAddr, amt),
	)

	// Create account if recipient does not exist.
	//
	// NOTE: This should ultimately be removed in favor a more flexible approach
	// such as delegated fee messages.
	accExists := k.ak.HasAccount(ctx, toAddr)
	if !accExists {
		defer telemetry.IncrCounter(1, "new", "account")
		k.ak.SetAccount(ctx, k.ak.NewAccountWithAddress(ctx, toAddr))
	}

	sdkCtx.EventManager().EmitEvents(sdk.Events{
		sdk.NewEvent(
			types.EventTypeTransfer,
			sdk.NewAttribute(types.AttributeKeyRecipient, toAddr.String()),
			sdk.NewAttribute(types.AttributeKeySender, fromAddr.String()),
			sdk.NewAttribute(sdk.AttributeKeyAmount, amt.String()),
		),
		sdk.NewEvent(
			sdk.EventTypeMessage,
			sdk.NewAttribute(types.AttributeKeySender, fromAddr.String()),
		),
	})

	return nil
}

// initBalances sets the balance (multiple coins) for an account by address.
// An error is returned upon failure.
func (k EVMSendKeeper) initBalances(ctx context.Context, addr sdk.AccAddress, balances sdk.Coins) error {
	return k.ek.MintCoins(ctx, addr, balances)
}

// IsSendEnabledCoins checks the coins provided and returns an ErrSendDisabled
// if any of the coins are not configured for sending. Returns nil if sending is
// enabled for all provided coins.
func (k EVMSendKeeper) IsSendEnabledCoins(ctx context.Context, coins ...sdk.Coin) error {
	if len(coins) == 0 {
		return nil
	}

	defaultVal := k.GetParams(ctx).DefaultSendEnabled

	for _, coin := range coins {
		if !k.getSendEnabledOrDefault(ctx, coin.Denom, defaultVal) {
			return types.ErrSendDisabled.Wrapf("%s transfers are currently disabled", coin.Denom)
		}
	}

	return nil
}

// IsSendEnabledCoin returns the current SendEnabled status of the provided coin's denom
func (k EVMSendKeeper) IsSendEnabledCoin(ctx context.Context, coin sdk.Coin) bool {
	return k.IsSendEnabledDenom(ctx, coin.Denom)
}

// BlockedAddr checks if a given address is restricted from
// receiving funds.
func (k EVMSendKeeper) BlockedAddr(addr sdk.AccAddress) bool {
	return k.blockedAddrs[addr.String()]
}

// GetBlockedAddresses returns the full list of addresses restricted from receiving funds.
func (k EVMSendKeeper) GetBlockedAddresses() map[string]bool {
	return k.blockedAddrs
}

// IsSendEnabledDenom returns the current SendEnabled status of the provided denom.
func (k EVMSendKeeper) IsSendEnabledDenom(ctx context.Context, denom string) bool {
	return k.getSendEnabledOrDefault(ctx, denom, k.GetParams(ctx).DefaultSendEnabled)
}

// GetSendEnabledEntry gets a SendEnabled entry for the given denom.
// The second return argument is true iff a specific entry exists for the given denom.
func (k EVMSendKeeper) GetSendEnabledEntry(ctx context.Context, denom string) (types.SendEnabled, bool) {
	sendEnabled, found := k.getSendEnabled(ctx, denom)
	if !found {
		return types.SendEnabled{}, false
	}

	return types.SendEnabled{Denom: denom, Enabled: sendEnabled}, true
}

// SetSendEnabled sets the SendEnabled flag for a denom to the provided value.
func (k EVMSendKeeper) SetSendEnabled(ctx context.Context, denom string, value bool) {
	_ = k.SendEnabled.Set(ctx, denom, value)
}

// SetAllSendEnabled sets all the provided SendEnabled entries in the bank store.
func (k EVMSendKeeper) SetAllSendEnabled(ctx context.Context, entries []*types.SendEnabled) {
	for _, entry := range entries {
		_ = k.SendEnabled.Set(ctx, entry.Denom, entry.Enabled)
	}
}

// DeleteSendEnabled deletes the SendEnabled flags for one or more denoms.
// If a denom is provided that doesn't have a SendEnabled entry, it is ignored.
func (k EVMSendKeeper) DeleteSendEnabled(ctx context.Context, denoms ...string) {
	for _, denom := range denoms {
		_ = k.SendEnabled.Remove(ctx, denom)
	}
}

// IterateSendEnabledEntries iterates over all the SendEnabled entries.
func (k EVMSendKeeper) IterateSendEnabledEntries(ctx context.Context, cb func(denom string, sendEnabled bool) bool) {
	err := k.SendEnabled.Walk(ctx, nil, func(key string, value bool) (stop bool, err error) {
		return cb(key, value), nil
	})
	if err != nil {
		panic(err)
	}
}

// GetAllSendEnabledEntries gets all the SendEnabled entries that are stored.
// Any denominations not returned use the default value (set in Params).
func (k EVMSendKeeper) GetAllSendEnabledEntries(ctx context.Context) []types.SendEnabled {
	var rv []types.SendEnabled
	k.IterateSendEnabledEntries(ctx, func(denom string, sendEnabled bool) bool {
		rv = append(rv, types.SendEnabled{Denom: denom, Enabled: sendEnabled})
		return false
	})

	return rv
}

// getSendEnabled returns whether send is enabled and whether that flag was set
// for a denom.
//
// Example usage:
//
//	store := ctx.KVStore(k.storeKey)
//	sendEnabled, found := getSendEnabled(store, "atom")
//	if !found {
//	    sendEnabled = DefaultSendEnabled
//	}
func (k EVMSendKeeper) getSendEnabled(ctx context.Context, denom string) (bool, bool) {
	has, err := k.SendEnabled.Has(ctx, denom)
	if err != nil || !has {
		return false, false
	}

	v, err := k.SendEnabled.Get(ctx, denom)
	if err != nil {
		return false, false
	}

	return v, true
}

// getSendEnabledOrDefault gets the SendEnabled value for a denom. If it's not
// in the store, this will return defaultVal.
func (k EVMSendKeeper) getSendEnabledOrDefault(ctx context.Context, denom string, defaultVal bool) bool {
	sendEnabled, found := k.getSendEnabled(ctx, denom)
	if found {
		return sendEnabled
	}

	return defaultVal
}

// sendRestriction is a struct that houses a SendRestrictionFn.
// It exists so that the SendRestrictionFn can be updated in the SendKeeper without needing to have a pointer receiver.
type sendRestriction struct {
	fn types.SendRestrictionFn
}

// newSendRestriction creates a new sendRestriction with nil send restriction.
func newSendRestriction() *sendRestriction {
	return &sendRestriction{
		fn: nil,
	}
}

// append adds the provided restriction to this, to be run after the existing function.
func (r *sendRestriction) append(restriction types.SendRestrictionFn) {
	r.fn = r.fn.Then(restriction)
}

// prepend adds the provided restriction to this, to be run before the existing function.
func (r *sendRestriction) prepend(restriction types.SendRestrictionFn) {
	r.fn = restriction.Then(r.fn)
}

// clear removes the send restriction (sets it to nil).
func (r *sendRestriction) clear() {
	r.fn = nil
}

var _ types.SendRestrictionFn = (*sendRestriction)(nil).apply

// apply applies the send restriction if there is one. If not, it's a no-op.
func (r *sendRestriction) apply(ctx context.Context, fromAddr, toAddr sdk.AccAddress, amt sdk.Coins) (sdk.AccAddress, error) {
	if r == nil || r.fn == nil {
		return toAddr, nil
	}
	return r.fn(ctx, fromAddr, toAddr, amt)
}
