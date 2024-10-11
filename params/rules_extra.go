// (c) 2024 Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package params

import (
	"github.com/ava-labs/coreth/precompile/precompileconfig"
	"github.com/ethereum/go-ethereum/common"
	gethparams "github.com/ethereum/go-ethereum/params"
)

func GetRulesExtra(r Rules) *RulesExtra {
	extra := FromRules(&r)
	return &extra
}

type RulesExtra struct {
	chainConfig *ChainConfig
	gethrules   gethparams.Rules

	// Rules for Avalanche releases
	AvalancheRules

	// Precompiles maps addresses to stateful precompiled contracts that are enabled
	// for this rule set.
	// Note: none of these addresses should conflict with the address space used by
	// any existing precompiles.
	Precompiles map[common.Address]precompileconfig.Config
	// Predicaters maps addresses to stateful precompile Predicaters
	// that are enabled for this rule set.
	Predicaters map[common.Address]precompileconfig.Predicater
	// AccepterPrecompiles map addresses to stateful precompile accepter functions
	// that are enabled for this rule set.
	AccepterPrecompiles map[common.Address]precompileconfig.Accepter
}

func (r *RulesExtra) PredicatersExist() bool {
	return len(r.Predicaters) > 0
}

func (r *RulesExtra) PredicaterExists(addr common.Address) bool {
	_, PredicaterExists := r.Predicaters[addr]
	return PredicaterExists
}

// IsPrecompileEnabled returns true if the precompile at [addr] is enabled for this rule set.
func (r *RulesExtra) IsPrecompileEnabled(addr common.Address) bool {
	_, ok := r.Precompiles[addr]
	return ok
}
