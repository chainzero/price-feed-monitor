package guardian

import (
	"fmt"

	"github.com/akash-network/price-feed-monitor/internal/config"
)

// buildSubmitCommand returns a copy-paste ready shell snippet to submit a guardian
// set upgrade VAA to the Akash Wormhole CosmWasm contract.
//
// Output format:
//
//	export VAA='<base64>'
//	akash tx wasm execute <contract> \
//	  "$(jq -nc --arg v "$VAA" '{submit_v_a_a:{vaa:$v}}')" \
//	  --from <operator> --chain-id <chain> --gas auto --gas-adjustment 1.5 -y
func buildSubmitCommand(network config.NetworkConfig, vaaBase64 string) string {
	cmd := fmt.Sprintf("export VAA='%s'\n", vaaBase64)
	cmd += fmt.Sprintf(
		"akash tx wasm execute %s \\\n"+
			"  \"$(jq -nc --arg v \"$VAA\" '{submit_v_a_a:{vaa:$v}}')\"",
		network.WormholeContract,
	)
	if network.OperatorAddress != "" {
		cmd += fmt.Sprintf(" \\\n  --from %s", network.OperatorAddress)
	}
	if network.ChainID != "" {
		cmd += fmt.Sprintf(" \\\n  --chain-id %s", network.ChainID)
	}
	cmd += " \\\n  --gas auto --gas-adjustment 1.5 -y\n"
	return cmd
}
