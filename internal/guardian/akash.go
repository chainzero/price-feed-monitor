package guardian

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// guardianSetInfoQuery is the base64-encoded CosmWasm smart query {"guardian_set_info": {}}.
// Used to query the Wormhole contract for the current on-chain guardian set.
const guardianSetInfoQuery = "eyJndWFyZGlhbl9zZXRfaW5mbyI6IHt9fQ=="

// AkashOracleClient queries the Akash Wormhole CosmWasm contract to retrieve
// the guardian addresses currently registered on-chain.
//
// NOTE: This is NOT the source of truth. It is what we compare against
// the authoritative Ethereum Wormhole contract data to detect drift.
type AkashOracleClient struct {
	apiURL           string
	network          string
	wormholeContract string
	client           *http.Client
}

func NewAkashOracleClient(apiURL, network, wormholeContract string) *AkashOracleClient {
	return &AkashOracleClient{
		apiURL:           apiURL,
		network:          network,
		wormholeContract: wormholeContract,
		client:           &http.Client{Timeout: 10 * time.Second},
	}
}

type cosmwasmGuardianSetResponse struct {
	Data struct {
		GuardianSetIndex uint32 `json:"guardian_set_index"`
		Addresses        []struct {
			Bytes string `json:"bytes"`
		} `json:"addresses"`
	} `json:"data"`
}

// GetGuardianAddresses fetches the guardian addresses currently registered in
// the Akash Wormhole CosmWasm contract. Returns addresses as lowercase hex
// without 0x prefix, matching the format produced by EthereumClient.GetGuardianSet.
func (c *AkashOracleClient) GetGuardianAddresses(ctx context.Context) ([]string, error) {
	if c.wormholeContract == "" {
		return nil, fmt.Errorf("wormhole_contract not configured for network %q", c.network)
	}

	url := fmt.Sprintf("%s/cosmwasm/wasm/v1/contract/%s/smart/%s",
		c.apiURL, c.wormholeContract, guardianSetInfoQuery)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d from Wormhole contract query", resp.StatusCode)
	}

	var result cosmwasmGuardianSetResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode guardian_set_info response: %w", err)
	}

	if len(result.Data.Addresses) == 0 {
		return nil, fmt.Errorf("guardian_set_info returned no addresses")
	}

	// Each address is a base64-encoded 20-byte raw Ethereum address.
	// Decode to hex (lowercase, no 0x prefix) to match the Ethereum client output.
	addresses := make([]string, len(result.Data.Addresses))
	for i, entry := range result.Data.Addresses {
		raw, err := base64.StdEncoding.DecodeString(entry.Bytes)
		if err != nil {
			return nil, fmt.Errorf("address[%d]: base64 decode %q: %w", i, entry.Bytes, err)
		}
		if len(raw) != 20 {
			return nil, fmt.Errorf("address[%d]: expected 20 bytes, got %d", i, len(raw))
		}
		addresses[i] = hex.EncodeToString(raw)
	}

	return addresses, nil
}
