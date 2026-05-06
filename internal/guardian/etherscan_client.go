package guardian

import (
	"bytes"
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

// submitNewGuardianSetSelector is the 4-byte function selector for submitNewGuardianSet(bytes).
// Computed as the first 4 bytes of keccak256("submitNewGuardianSet(bytes)").
// Used to identify guardian set upgrade transactions in the Etherscan txlist response.
const submitNewGuardianSetSelector = "3bc0aee6"

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
// a submitNewGuardianSet call that upgrades TO targetIndex. Returns the VAA as base64
// and the Ethereum transaction hash for operator verification.
//
// How it works:
//  1. Fetch the last 50 transactions to the Wormhole contract via Etherscan txlist.
//  2. Filter for submitNewGuardianSet calls (selector 0x3bc0aee6).
//  3. ABI-decode the `bytes _vm` argument from the calldata.
//  4. Pre-filter by signing index: for an upgrade TO targetIndex, the signing set is targetIndex-1.
//  5. Validate the VAA payload: must be Core governance action type 2 targeting targetIndex.
//  6. Return the matching VAA bytes as base64 and the source transaction hash.
func (c *EtherscanClient) GetGuardianSetUpgradeVAA(ctx context.Context, targetIndex uint32) (vaaBase64, txHash string, err error) {
	if c.apiKey == "" {
		return "", "", fmt.Errorf("etherscan API key not configured")
	}

	url := fmt.Sprintf(
		"https://api.etherscan.io/v2/api?chainid=1&module=account&action=txlist"+
			"&address=%s&sort=desc&page=1&offset=50&apikey=%s",
		c.wormholeContract, c.apiKey,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", err
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("etherscan txlist request: %w", err)
	}
	defer resp.Body.Close()

	var result etherscanTxListResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", fmt.Errorf("decode etherscan response: %w", err)
	}
	if result.Status != "1" {
		return "", "", fmt.Errorf("etherscan API error: %s", result.Message)
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
		if vaaSigningIndex != signingIndex {
			continue
		}

		// Validate the payload is a Core governance guardian set upgrade to targetIndex.
		// This guards against other governance transaction types (e.g. DelegatedGuardians)
		// that may share the same signing index but are not guardian set rotation VAAs.
		if err := validateGuardianSetUpgradeVAA(vaaBytes, targetIndex); err != nil {
			continue
		}

		return base64.StdEncoding.EncodeToString(vaaBytes), tx.Hash, nil
	}

	return "", "", fmt.Errorf("no submitNewGuardianSet transaction found for target index %d (signing index %d) in last 50 transactions",
		targetIndex, signingIndex)
}

// validateGuardianSetUpgradeVAA verifies that vaaBytes is a Wormhole Core governance
// guardian set upgrade VAA that promotes the set to newIndex.
//
// VAA layout (all offsets from byte 0):
//
//	[0]              version (1 byte)
//	[1:5]            signing guardian_set_index (4 bytes)
//	[5]              num_signatures (1 byte)
//	[6 + i*66]       signature i: guardian_index(1) + sig(65) = 66 bytes each
//	[6 + n*66]       body: timestamp(4)+nonce(4)+emitter_chain(2)+emitter_addr(32)+sequence(8)+consistency(1)
//	[6 + n*66 + 51]  payload: module(32)+action(1)+chain(2)+new_gs_index(4)+...
func validateGuardianSetUpgradeVAA(vaaBytes []byte, newIndex uint32) error {
	if len(vaaBytes) < 6 {
		return fmt.Errorf("VAA too short (%d bytes)", len(vaaBytes))
	}
	numSigs := int(vaaBytes[5])
	payloadStart := 6 + numSigs*66 + 51 // header + body (51 = 4+4+2+32+8+1)

	const payloadMinLen = 32 + 1 + 2 + 4 // module + action + chain + new_gs_index
	if len(vaaBytes) < payloadStart+payloadMinLen {
		return fmt.Errorf("VAA too short for governance payload (have %d bytes, need %d)",
			len(vaaBytes), payloadStart+payloadMinLen)
	}

	// Signing guardian set index must be newIndex-1. Wormhole enforces sequential
	// rotation, and the Akash contract rejects VAAs signed by an expired set.
	signingIndex := binary.BigEndian.Uint32(vaaBytes[1:5])
	if signingIndex != newIndex-1 {
		return fmt.Errorf("VAA signing index %d does not match expected %d (newIndex-1)", signingIndex, newIndex-1)
	}

	payload := vaaBytes[payloadStart:]

	// Core module identifier: 28 zero bytes followed by the ASCII string "Core".
	var coreModule [32]byte
	copy(coreModule[28:], []byte("Core"))
	if !bytes.Equal(payload[:32], coreModule[:]) {
		return fmt.Errorf("not a Core governance VAA (module: %x)", payload[:32])
	}

	// Action type 2 = guardian set upgrade (1 = contract upgrade, 3 = set message fee).
	if payload[32] != 0x02 {
		return fmt.Errorf("VAA action type %d is not a guardian set upgrade (expected 2)", payload[32])
	}

	// New guardian set index is at payload[35:39]: after module(32) + action(1) + chain(2).
	gotIndex := binary.BigEndian.Uint32(payload[35:39])
	if gotIndex != newIndex {
		return fmt.Errorf("VAA upgrades to guardian set index %d, expected %d", gotIndex, newIndex)
	}

	return nil
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
