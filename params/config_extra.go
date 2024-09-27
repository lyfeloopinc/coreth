// (c) 2024 Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package params

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"

	"github.com/ava-labs/avalanchego/snow"
	"github.com/ava-labs/avalanchego/upgrade"
	"github.com/ava-labs/coreth/utils"
	"github.com/ethereum/go-ethereum/common"
)

const (
	maxJSONLen = 64 * 1024 * 1024 // 64MB

	// XXX: Value to pass to geth's Rules by default where the appropriate
	// context is not available in the avalanche code. (similar to context.TODO())
	IsMergeTODO = true
)

// UpgradeConfig includes the following configs that may be specified in upgradeBytes:
// - Timestamps that enable avalanche network upgrades,
// - Enabling or disabling precompiles as network upgrades.
type UpgradeConfig struct {
	// Config for enabling and disabling precompiles as network upgrades.
	PrecompileUpgrades []PrecompileUpgrade `json:"precompileUpgrades,omitempty"`
}

// AvalancheContext provides Avalanche specific context directly into the EVM.
type AvalancheContext struct {
	SnowCtx *snow.Context
}

// SetEthUpgrades enables Etheruem network upgrades using the same time as
// the Avalanche network upgrade that enables them.
//
// TODO: Prior to Cancun, Avalanche upgrades are referenced inline in the
// code in place of their Ethereum counterparts. The original Ethereum names
// should be restored for maintainability.
func SetEthUpgrades(c *ChainConfig) {
	extra := GetExtra(c)
	if extra.DurangoBlockTimestamp != nil {
		c.ShanghaiTime = utils.NewUint64(*extra.DurangoBlockTimestamp)
	}
	if extra.EtnaTimestamp != nil {
		c.CancunTime = utils.NewUint64(*extra.EtnaTimestamp)
	}
}

// UnmarshalJSON parses the JSON-encoded data and stores the result in the
// object pointed to by c.
// This is a custom unmarshaler to handle the Precompiles field.
// Precompiles was presented as an inline object in the JSON.
// This custom unmarshaler ensures backwards compatibility with the old format.
func (c *ChainConfigExtra) UnmarshalJSON(data []byte) error {
	// Alias ChainConfigExtra to avoid recursion
	type _ChainConfigExtra ChainConfigExtra
	tmp := _ChainConfigExtra{}
	if err := json.Unmarshal(data, &tmp); err != nil {
		return err
	}

	// At this point we have populated all fields except PrecompileUpgrade
	*c = ChainConfigExtra(tmp)

	return nil
}

// MarshalJSON returns the JSON encoding of c.
// This is a custom marshaler to handle the Precompiles field.
func (c *ChainConfigExtra) MarshalJSON() ([]byte, error) {
	// Alias ChainConfigExtra to avoid recursion
	type _ChainConfigExtra ChainConfigExtra
	return json.Marshal(_ChainConfigExtra(*c))
}

type ChainConfigWithUpgradesJSON struct {
	ChainConfig
	UpgradeConfig UpgradeConfig `json:"upgrades,omitempty"`
}

// MarshalJSON implements json.Marshaler. This is a workaround for the fact that
// the embedded ChainConfig struct has a MarshalJSON method, which prevents
// the default JSON marshalling from working for UpgradeConfig.
// TODO: consider removing this method by allowing external tag for the embedded
// ChainConfig struct.
func (cu ChainConfigWithUpgradesJSON) MarshalJSON() ([]byte, error) {
	// embed the ChainConfig struct into the response
	chainConfigJSON, err := json.Marshal(&cu.ChainConfig) // XXX: Marshal should be defined on value receiver?
	if err != nil {
		return nil, err
	}
	if len(chainConfigJSON) > maxJSONLen {
		return nil, errors.New("value too large")
	}

	type upgrades struct {
		UpgradeConfig UpgradeConfig `json:"upgrades"`
	}

	upgradeJSON, err := json.Marshal(upgrades{cu.UpgradeConfig})
	if err != nil {
		return nil, err
	}
	if len(upgradeJSON) > maxJSONLen {
		return nil, errors.New("value too large")
	}

	// merge the two JSON objects
	mergedJSON := make([]byte, 0, len(chainConfigJSON)+len(upgradeJSON)+1)
	mergedJSON = append(mergedJSON, chainConfigJSON[:len(chainConfigJSON)-1]...)
	mergedJSON = append(mergedJSON, ',')
	mergedJSON = append(mergedJSON, upgradeJSON[1:]...)
	return mergedJSON, nil
}

func (cu *ChainConfigWithUpgradesJSON) UnmarshalJSON(input []byte) error {
	var cc ChainConfig
	if err := json.Unmarshal(input, &cc); err != nil {
		return err
	}

	type upgrades struct {
		UpgradeConfig UpgradeConfig `json:"upgrades"`
	}

	var u upgrades
	if err := json.Unmarshal(input, &u); err != nil {
		return err
	}
	cu.ChainConfig = cc
	cu.UpgradeConfig = u.UpgradeConfig
	return nil
}

// Verify verifies chain config and returns error
func (c *ChainConfigExtra) Verify() error {
	// Verify the precompile upgrades are internally consistent given the existing chainConfig.
	if err := c.verifyPrecompileUpgrades(); err != nil {
		return fmt.Errorf("invalid precompile upgrades: %w", err)
	}

	return nil
}

// IsPrecompileEnabled returns whether precompile with [address] is enabled at [timestamp].
func (c *ChainConfigExtra) IsPrecompileEnabled(address common.Address, timestamp uint64) bool {
	config := c.getActivePrecompileConfig(address, timestamp)
	return config != nil && !config.IsDisabled()
}

// ToWithUpgradesJSON converts the ChainConfig to ChainConfigWithUpgradesJSON with upgrades explicitly displayed.
// ChainConfig does not include upgrades in its JSON output.
// This is a workaround for showing upgrades in the JSON output.
func ToWithUpgradesJSON(c *ChainConfig) *ChainConfigWithUpgradesJSON {
	return &ChainConfigWithUpgradesJSON{
		ChainConfig:   *c,
		UpgradeConfig: GetExtra(c).UpgradeConfig,
	}
}

func GetChainConfig(agoUpgrade upgrade.Config, chainID *big.Int) *ChainConfig {
	c := WithExtra(
		&ChainConfig{
			ChainID:             chainID,
			HomesteadBlock:      big.NewInt(0),
			DAOForkBlock:        big.NewInt(0),
			DAOForkSupport:      true,
			EIP150Block:         big.NewInt(0),
			EIP155Block:         big.NewInt(0),
			EIP158Block:         big.NewInt(0),
			ByzantiumBlock:      big.NewInt(0),
			ConstantinopleBlock: big.NewInt(0),
			PetersburgBlock:     big.NewInt(0),
			IstanbulBlock:       big.NewInt(0),
			MuirGlacierBlock:    big.NewInt(0),
		},
		&ChainConfigExtra{
			NetworkUpgrades: getNetworkUpgrades(agoUpgrade),
		},
	)
	if AvalancheFujiChainID.Cmp(c.ChainID) == 0 {
		c.BerlinBlock = big.NewInt(184985) // https://testnet.snowtrace.io/block/184985?chainid=43113, AP2 activation block
		c.LondonBlock = big.NewInt(805078) // https://testnet.snowtrace.io/block/805078?chainid=43113, AP3 activation block
	} else if AvalancheMainnetChainID.Cmp(c.ChainID) == 0 {
		c.BerlinBlock = big.NewInt(1640340) // https://snowtrace.io/block/1640340?chainid=43114, AP2 activation block
		c.LondonBlock = big.NewInt(3308552) // https://snowtrace.io/block/3308552?chainid=43114, AP3 activation block
	} else {
		// In testing or local networks, we only support enabling Berlin and London prior
		// to the initially active time. This is likely to correspond to an intended block
		// number of 0 as well.
		initiallyActive := uint64(upgrade.InitiallyActiveTime.Unix())
		extra := GetExtra(c)
		if extra.ApricotPhase2BlockTimestamp != nil && *extra.ApricotPhase2BlockTimestamp <= initiallyActive && c.BerlinBlock == nil {
			c.BerlinBlock = big.NewInt(0)
		}
		if extra.ApricotPhase3BlockTimestamp != nil && *extra.ApricotPhase3BlockTimestamp <= initiallyActive && c.LondonBlock == nil {
			c.LondonBlock = big.NewInt(0)
		}
	}
	return c
}

func (r *RulesExtra) PredicatersExist() bool {
	// Methods on *RulesExtra handle nil receiver so params.Rules is an initialized struct.
	if r == nil {
		return false
	}
	return len(r.Predicaters) > 0
}

func (r *RulesExtra) PredicaterExists(addr common.Address) bool {
	// Methods on *RulesExtra handle nil receiver so params.Rules is an initialized struct.
	if r == nil {
		return false
	}
	_, PredicaterExists := r.Predicaters[addr]
	return PredicaterExists
}

// IsPrecompileEnabled returns true if the precompile at [addr] is enabled for this rule set.
func (r *RulesExtra) IsPrecompileEnabled(addr common.Address) bool {
	// Methods on *RulesExtra handle nil receiver so params.Rules is an initialized struct.
	if r == nil {
		return false
	}
	_, ok := r.Precompiles[addr]
	return ok
}

func ptrToString(val *uint64) string {
	if val == nil {
		return "nil"
	}
	return fmt.Sprintf("%d", *val)
}

// IsForkTransition returns true if [fork] activates during the transition from
// [parent] to [current].
// Taking [parent] as a pointer allows for us to pass nil when checking forks
// that activate during genesis.
// Note: this works for both block number and timestamp activated forks.
func IsForkTransition(fork *uint64, parent *uint64, current uint64) bool {
	var parentForked bool
	if parent != nil {
		parentForked = isTimestampForked(fork, *parent)
	}
	currentForked := isTimestampForked(fork, current)
	return !parentForked && currentForked
}

func WithExtra(c *ChainConfig, extra *ChainConfigExtra) *ChainConfig {
	// XXX: Hack to initialize the ChainConfigExtra pointer in the ChainConfig.
	jsonBytes, err := json.Marshal(c)
	if err != nil {
		panic(err)
	}
	var newCfg ChainConfig
	if err := json.Unmarshal(jsonBytes, &newCfg); err != nil {
		panic(err)
	}

	*GetExtra(&newCfg) = *extra
	return &newCfg
}
