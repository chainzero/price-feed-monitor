package guardian

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// WormholescanClient queries the Wormholescan REST API for guardian set information
// and governance VAAs.
//
// Wormholescan is the canonical public indexer for the Wormhole network. It serves
// two purposes for this monitor:
//
//  1. Current global guardian set index and addresses — used to detect when a
//     rotation has occurred by comparing against what Akash has on-chain.
//
//  2. Governance VAAs — the signed byte payloads produced when guardians vote to
//     rotate the guardian set. These bytes are the exact input required by the
//     Akash Wormhole CosmWasm contract's submit_v_a_a execution message.
//
// Why Wormholescan instead of (or in addition to) Ethereum RPC:
//   - No Ethereum node or API key required — pure REST calls to a public API.
//   - Returns the governance VAA directly as base64, ready for on-chain submission.
//   - Historical VAA retrieval: if our monitor missed the initial rotation event,
//     Wormholescan retains the VAA indefinitely, so we can still retrieve it.
//
// Wormholescan API versions:
//   - /v1/  — newer REST endpoints (guardian set queries)
//   - /api/v1/ — older REST endpoints (VAA queries); both remain in active use.
type WormholescanClient struct {
	baseURL string
	client  *http.Client
}

func NewWormholescanClient(baseURL string) *WormholescanClient {
	return &WormholescanClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: 15 * time.Second},
	}
}

// --- API response types ---

// currentGuardianSetResponse is the JSON response from GET /v1/guardianset/current.
//
// The "index" field is the globally active guardian set number. This is the value
// we compare against what is stored in the Akash Wormhole CosmWasm contract to
// determine whether an update is needed.
//
// Addresses are returned without the "0x" prefix and in lowercase hex. They match
// the 20-byte Ethereum address format used by the Wormhole guardian keys.
type currentGuardianSetResponse struct {
	GuardianSet struct {
		Index     uint32 `json:"index"`
		Addresses []struct {
			// Address is a 20-byte hex Ethereum address, e.g. "58cc3ae5c097b213ce3c81979e1b9f9d5929b6ab"
			Address string `json:"address"`
		} `json:"addresses"`
	} `json:"guardianSet"`
}

// guardianSetVAAEntry represents a single governance VAA in the Wormholescan history.
//
// The guardian set upgrade sequence is:
//   - GuardianSetIndex: the index of the guardian set that SIGNED this VAA
//     (i.e., the outgoing set authorising the rotation)
//   - The VAA payload encodes the NEW guardian set index (SigningIndex + 1 for
//     sequential rotations, which the Wormhole governance protocol enforces)
//   - VAA: base64-encoded signed bytes, ready for direct submission to submit_v_a_a
//
// Historical record (as of 2026-03):
//
//	GuardianSetIndex  Rotation           Date
//	4                 Set 4 → Set 5      2026-03-15
//	3                 Set 3 → Set 4      2024-04-16
//	2                 Set 2 → Set 3      2023-01-16
//	1                 Set 1 → Set 2      2022-05-01
type guardianSetVAAEntry struct {
	// GuardianSetIndex is the SIGNING set's index, not the upgrade target index.
	// To find the VAA that upgrades TO index N, search for GuardianSetIndex == N-1.
	GuardianSetIndex uint32 `json:"guardianSetIndex"`

	// VAA contains the complete governance VAA as a base64-encoded string.
	// Governance module: "Core" (bytes 0x60–0x63)
	// Action type: 0x02 (guardian set upgrade) at byte 0x70
	// New index: encoded as uint16 at bytes 0x76–0x77
	// Guardian count: encoded at byte 0x78 (19 = 0x13 for current sets)
	VAA string `json:"vaa"`

	// Timestamp is the VAA publication time in RFC3339 format. Used to calculate
	// how much of the 24-hour grace period has already elapsed.
	Timestamp string `json:"timestamp"`
}

// vaaListResponse is the paginated JSON response from GET /api/v1/vaas/{chain}/{emitter}.
type vaaListResponse struct {
	Data []guardianSetVAAEntry `json:"data"`
}

// --- Client methods ---

// GetCurrentGuardianSet queries the Wormholescan API for the globally active guardian
// set. Returns the index and the normalized (lowercase, no "0x" prefix) addresses.
func (c *WormholescanClient) GetCurrentGuardianSet(ctx context.Context) (uint32, []string, error) {
	url := c.baseURL + "/v1/guardianset/current"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, nil, fmt.Errorf("wormholescan returned HTTP %d for guardian set", resp.StatusCode)
	}

	var result currentGuardianSetResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, nil, fmt.Errorf("decode guardian set response: %w", err)
	}

	addresses := make([]string, len(result.GuardianSet.Addresses))
	for i, a := range result.GuardianSet.Addresses {
		// Normalize: strip optional "0x" prefix and lowercase for consistent comparison
		// against the addresses returned by GetGuardianAddresses (Akash oracle params).
		addr := strings.TrimPrefix(strings.ToLower(a.Address), "0x")
		addresses[i] = addr
	}

	return result.GuardianSet.Index, addresses, nil
}

// GetUpgradeVAA retrieves the governance VAA that upgrades the guardian set TO
// the specified target index.
//
// The Wormhole governance protocol enforces sequential guardian set indices, so
// the VAA upgrading TO index N is always SIGNED BY guardian set N-1. This method
// searches the governance emitter's VAA history for that entry.
//
// Parameters:
//   - governanceEmitter: the Wormhole Core governance emitter address, padded to
//     64 hex chars. Standard value: 0000...0004 (the Wormhole governance emitter
//     on Ethereum chain 1 for all guardian set upgrades since mainnet launch).
//   - targetIndex: the new guardian set index we want the VAA for.
//
// Returns the VAA as a base64 string and the VAA's publication timestamp.
func (c *WormholescanClient) GetUpgradeVAA(ctx context.Context, governanceEmitter string, targetIndex uint32) (vaaBase64, timestamp string, err error) {
	// Chain 1 = Ethereum mainnet. The governance emitter is a well-known constant
	// address used for all Wormhole core governance actions (guardian set upgrades,
	// contract upgrades, fee updates, etc.).
	url := fmt.Sprintf("%s/api/v1/vaas/1/%s?pageSize=20", c.baseURL, governanceEmitter)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("wormholescan returned HTTP %d for VAA list", resp.StatusCode)
	}

	var result vaaListResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", fmt.Errorf("decode VAA list response: %w", err)
	}

	if len(result.Data) == 0 {
		return "", "", fmt.Errorf("no governance VAAs found in response")
	}

	// Search for the VAA signed by guardian set (targetIndex - 1).
	// This is the governance VAA that upgrades TO targetIndex.
	signingIndex := targetIndex - 1
	for _, entry := range result.Data {
		if entry.GuardianSetIndex == signingIndex {
			return entry.VAA, entry.Timestamp, nil
		}
	}

	// Fallback: if we can't find the exact match (e.g., non-sequential rotation or
	// response doesn't include it yet), return the most recent VAA with a warning.
	// The caller logs this situation and includes a note in the alert.
	return result.Data[0].VAA, result.Data[0].Timestamp, fmt.Errorf(
		"exact VAA for target index %d (signing index %d) not found — using most recent VAA (signing index %d)",
		targetIndex, signingIndex, result.Data[0].GuardianSetIndex,
	)
}
