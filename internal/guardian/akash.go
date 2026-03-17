package guardian

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// AkashOracleClient queries the Akash oracle module params to retrieve the
// guardian addresses currently configured on-chain.
//
// NOTE: This is NOT the source of truth. It is what we compare against
// the authoritative Ethereum Wormhole contract data to detect drift.
type AkashOracleClient struct {
	apiURL  string
	network string
	client  *http.Client
}

func NewAkashOracleClient(apiURL, network string) *AkashOracleClient {
	return &AkashOracleClient{
		apiURL:  apiURL,
		network: network,
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

type oracleParamsResponse struct {
	Params struct {
		FeedContractsParams []json.RawMessage `json:"feed_contracts_params"`
	} `json:"params"`
}

type wormholeContractParams struct {
	Type              string   `json:"@type"`
	GuardianAddresses []string `json:"guardian_addresses"`
}

// GetGuardianAddresses fetches the guardian addresses currently registered in
// Akash oracle params. Returns addresses as lowercase hex without 0x prefix,
// matching the format produced by EthereumClient.GetGuardianSet.
func (c *AkashOracleClient) GetGuardianAddresses(ctx context.Context) ([]string, error) {
	url := fmt.Sprintf("%s/akash/oracle/v1/params", c.apiURL)
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
		return nil, fmt.Errorf("unexpected status %d from oracle params API", resp.StatusCode)
	}

	var result oracleParamsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode oracle params: %w", err)
	}

	for _, raw := range result.Params.FeedContractsParams {
		var params wormholeContractParams
		if err := json.Unmarshal(raw, &params); err != nil {
			continue
		}
		if params.Type != "/akash.oracle.v1.WormholeContractParams" {
			continue
		}
		if len(params.GuardianAddresses) == 0 {
			return nil, fmt.Errorf("WormholeContractParams found but guardian_addresses is empty")
		}
		// Normalize to lowercase to match Ethereum client output.
		normalized := make([]string, len(params.GuardianAddresses))
		for i, addr := range params.GuardianAddresses {
			normalized[i] = strings.ToLower(addr)
		}
		return normalized, nil
	}

	return nil, fmt.Errorf("WormholeContractParams not found in oracle params")
}
