package guardian

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	// Function selectors for the Wormhole core contract.
	selectorGetCurrentIndex = "0x1cfe7951"
	selectorGetGuardianSet  = "0xf951975a"
)

type ethRPCRequest struct {
	JSONRPC string        `json:"jsonrpc"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
	ID      int           `json:"id"`
}

type ethRPCResponse struct {
	Result string       `json:"result"`
	Error  *ethRPCError `json:"error"`
}

type ethRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// EthereumClient queries the Wormhole core contract via raw JSON-RPC eth_call.
type EthereumClient struct {
	rpcURL           string
	wormholeContract string
	client           *http.Client
}

func NewEthereumClient(rpcURL, wormholeContract string) *EthereumClient {
	return &EthereumClient{
		rpcURL:           rpcURL,
		wormholeContract: wormholeContract,
		client:           &http.Client{Timeout: 15 * time.Second},
	}
}

// GetGuardianSetIndex calls getCurrentGuardianSetIndex() on the Wormhole contract.
// This is the authoritative source of truth for the current guardian set.
func (c *EthereumClient) GetGuardianSetIndex(ctx context.Context) (uint32, error) {
	result, err := c.ethCall(ctx, selectorGetCurrentIndex)
	if err != nil {
		return 0, err
	}
	return decodeUint32(result)
}

// GetGuardianSet calls getGuardianSet(uint32) on the Wormhole contract and
// returns the guardian addresses as lowercase hex strings without 0x prefix.
func (c *EthereumClient) GetGuardianSet(ctx context.Context, index uint32) ([]string, error) {
	callData := fmt.Sprintf("%s%064x", selectorGetGuardianSet, index)
	result, err := c.ethCall(ctx, callData)
	if err != nil {
		return nil, err
	}
	return decodeGuardianAddresses(result)
}

func (c *EthereumClient) ethCall(ctx context.Context, data string) (string, error) {
	payload := ethRPCRequest{
		JSONRPC: "2.0",
		Method:  "eth_call",
		Params: []interface{}{
			map[string]string{
				"to":   c.wormholeContract,
				"data": data,
			},
			"latest",
		},
		ID: 1,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.rpcURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("eth_call request: %w", err)
	}
	defer resp.Body.Close()

	var rpcResp ethRPCResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return "", fmt.Errorf("decode RPC response: %w", err)
	}
	if rpcResp.Error != nil {
		return "", fmt.Errorf("RPC error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}

	return rpcResp.Result, nil
}

// decodeUint32 decodes a 32-byte ABI-encoded uint32 result.
func decodeUint32(hexStr string) (uint32, error) {
	b, err := hex.DecodeString(strings.TrimPrefix(hexStr, "0x"))
	if err != nil {
		return 0, fmt.Errorf("decode hex: %w", err)
	}
	if len(b) < 32 {
		return 0, fmt.Errorf("result too short: %d bytes", len(b))
	}
	return binary.BigEndian.Uint32(b[28:32]), nil
}

// decodeGuardianAddresses decodes the ABI-encoded return value of getGuardianSet(uint32).
// The return type is a tuple (address[] keys, uint32 expirationTime).
//
// Verified layout from live Ethereum mainnet response:
//
//	Word 0 [chars   0- 63]: offset to tuple = 32
//	Word 1 [chars  64-127]: offset to address[] within tuple = 64
//	Word 2 [chars 128-191]: expirationTime (0 for the active set)
//	Word 3 [chars 192-255]: length of address array (19)
//	Words 4+ [chars 256+] : addresses, 32 bytes each, address in last 20 bytes
func decodeGuardianAddresses(hexStr string) ([]string, error) {
	data := strings.TrimPrefix(hexStr, "0x")
	const wordChars = 64 // 32 bytes = 64 hex chars per word

	if len(data) < 4*wordChars {
		return nil, fmt.Errorf("response too short: %d chars (need %d)", len(data), 4*wordChars)
	}

	// Word 3: array length
	lenBytes, err := hex.DecodeString(data[3*wordChars : 4*wordChars])
	if err != nil {
		return nil, fmt.Errorf("decode array length: %w", err)
	}
	count := int(binary.BigEndian.Uint32(lenBytes[28:]))

	if count == 0 {
		return nil, fmt.Errorf("guardian set returned 0 addresses")
	}
	if len(data) < (4+count)*wordChars {
		return nil, fmt.Errorf("response too short for %d addresses", count)
	}

	addresses := make([]string, count)
	for i := 0; i < count; i++ {
		start := (4 + i) * wordChars
		word := data[start : start+wordChars]
		// Each address word is 32 bytes; the address occupies the last 20 bytes.
		// That's the last 40 hex chars of the 64-char word.
		addresses[i] = strings.ToLower(word[24:])
	}
	return addresses, nil
}
