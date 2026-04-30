package guardian

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// submitNewGuardianSetSelector is the first 4 bytes of keccak256("submitNewGuardianSet(bytes)").
// Used to identify guardian set upgrade transactions in the Etherscan txlist response.
const submitNewGuardianSetSelector = "6606b4e0"

// EtherscanClient retrieves guardian set upgrade VAAs from Ethereum transaction calldata.
//
// Background: The Wormhole core contract's submitNewGuardianSet(bytes _vm) accepts the
// governance VAA as its sole argument. This VAA is the exact bytes needed for the Akash
// submit_v_a_a call. Etherscan's txlist endpoint lets us find this transaction and extract
// the calldata — no Ethereum archive node or event log required.
//
// Note: GuardianSetAdded(uint32) is declared in Governance.sol but never emitted, so
// eth_getLogs cannot be used to locate the upgrade transaction.
type EtherscanClient struct {
	apiKey           string
	wormholeContract string
	client           *http.Client
}

func NewEtherscanClient(apiKey, wormholeContract string) *EtherscanClient {
	return &EtherscanClient{
		apiKey:           apiKey,
		wormholeContract: wormholeContract,
		client:           &http.Client{Timeout: 15 * time.Second},
	}
}

type etherscanTxListResponse struct {
	Status  string            `json:"status"`
	Message string            `json:"message"`
	Result  []etherscanTx     `json:"result"`
}

type etherscanTx struct {
	Hash         string `json:"hash"`
	BlockNumber  string `json:"blockNumber"`
	TimeStamp    string `json:"timeStamp"`
	Input        string `json:"input"`
	FunctionName string `json:"functionName"`
	IsError      string `json:"isError"`
}

// GetGuardianSetUpgradeVAA searches recent transactions to the Wormhole contract for
// a submitNewGuardianSet call that upgrades TO targetIndex. Returns the VAA as base64.
//
// How it works:
//  1. Fetch the last 50 transactions to the Wormhole contract via Etherscan txlist.
//  2. Filter for submitNewGuardianSet calls (selector 0x6606b4e0).
//  3. ABI-decode the `bytes _vm` argument from the calldata.
//  4. Parse the VAA header: the guardian_set_index field is the SIGNING set (targetIndex-1).
//     This identifies which VAA corresponds to the desired upgrade.
//  5. Return the matching VAA bytes as base64.
func (c *EtherscanClient) GetGuardianSetUpgradeVAA(ctx context.Context, targetIndex uint32) (string, error) {
	if c.apiKey == "" {
		return "", fmt.Errorf("etherscan API key not configured")
	}

	url := fmt.Sprintf(
		"https://api.etherscan.io/v2/api?chainid=1&module=account&action=txlist"+
			"&address=%s&sort=desc&page=1&offset=50&apikey=%s",
		c.wormholeContract, c.apiKey,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("etherscan txlist request: %w", err)
	}
	defer resp.Body.Close()

	var result etherscanTxListResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode etherscan response: %w", err)
	}
	if result.Status != "1" {
		return "", fmt.Errorf("etherscan API error: %s", result.Message)
	}

	signingIndex := targetIndex - 1

	for _, tx := range result.Result {
		if tx.IsError == "1" {
			continue
		}
		input := strings.TrimPrefix(strings.ToLower(tx.Input), "0x")
		if len(input) < 8 || input[:8] != submitNewGuardianSetSelector {
			continue
		}

		vaaBytes, err := decodeVAAFromCalldata(input)
		if err != nil {
			continue
		}

		// VAA header: version(1) + guardian_set_index(4) + ...
		// guardian_set_index is the set that SIGNED this VAA (i.e. the outgoing set).
		// For an upgrade TO targetIndex, the signing set is targetIndex-1.
		if len(vaaBytes) < 5 {
			continue
		}
		vaaSigningIndex := binary.BigEndian.Uint32(vaaBytes[1:5])
		if vaaSigningIndex == signingIndex {
			return base64.StdEncoding.EncodeToString(vaaBytes), nil
		}
	}

	return "", fmt.Errorf("no submitNewGuardianSet transaction found for target index %d (signing index %d) in last 50 transactions",
		targetIndex, signingIndex)
}

// decodeVAAFromCalldata decodes the `bytes _vm` ABI argument from submitNewGuardianSet calldata.
//
// ABI encoding of submitNewGuardianSet(bytes _vm) after stripping the 4-byte selector:
//
//	[0x00–0x1f] offset to bytes data (always 0x20 = 32)
//	[0x20–0x3f] length of bytes data in bytes
//	[0x40–...] bytes data (the raw VAA)
func decodeVAAFromCalldata(inputHex string) ([]byte, error) {
	// Strip 4-byte selector (8 hex chars), leaving only the ABI-encoded parameters.
	params := inputHex[8:]

	// Minimum: offset word (64) + length word (64) + at least 1 byte of data (2)
	if len(params) < 128 {
		return nil, fmt.Errorf("calldata too short: %d hex chars", len(params))
	}

	// Word 1 (offset): always 0x20, skip it.
	// Word 2 (length): bytes 32–63 of params.
	lenBytes, err := hex.DecodeString(params[64:128])
	if err != nil {
		return nil, fmt.Errorf("decode length word: %w", err)
	}
	dataLen := int(binary.BigEndian.Uint32(lenBytes[28:32]))

	// The actual VAA bytes start at offset 128 (after the two 64-char words).
	dataHex := params[128:]
	if len(dataHex) < dataLen*2 {
		return nil, fmt.Errorf("calldata truncated: need %d bytes, have %d hex chars", dataLen, len(dataHex))
	}

	vaaBytes, err := hex.DecodeString(dataHex[:dataLen*2])
	if err != nil {
		return nil, fmt.Errorf("decode VAA bytes: %w", err)
	}
	return vaaBytes, nil
}
